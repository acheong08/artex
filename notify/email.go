package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout bound connection setup and the entire SMTP
// session, respectively. net/smtp has no built-in timeouts; without these limits, a
// stuck peer would hang a delivery goroutine forever. Since the dispatcher processes
// messages serially in one goroutine, that would stop the entire notification system.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP email delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but should not be used to flood recipients; provide a generous default.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Mask only the password. SMTP host, account, and recipients are not secret; masking
// them would only make editing more cumbersome.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port determine which server receives the password; tls controls whether it is
// transmitted securely. Changing any of the three requires explicitly resubmitting the
// password, including when disabling TLS.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP server address is required")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("invalid SMTP port (must be 1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("sender address is required")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("at least one recipient address is required")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// Upgrade with STARTTLS if the peer supports it. Credentials must not be sent over
	// a plaintext connection (see the auth explanation below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials over an unencrypted connection
			// (except to localhost). This is **correct** security behavior and must not be
			// bypassed, but explain the reason clearly so users know what to do instead of
			// seeing only "unencrypted connection."
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials over an unencrypted connection. Enable TLS, use port 465 (implicit TLS), or check “Enable TLS” (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP authentication failed: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("sender %s was rejected", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("recipient %s was rejected", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("failed to write email body: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("failed to submit email: %w", err)
	}
	// A Quit failure does not change the fact that the server accepted the email, so ignore it.
	_ = client.Quit()
	// Email bodies (HTML) are not truncated, so count the entire batch as delivered.
	return len(m.Items), nil
}

// emailDial establishes an SMTP connection.
//
// implicitTLS=true uses immediate TLS (as on port 465); false connects in plaintext on
// ports 25/587 and upgrades with STARTTLS. Do not mix them: a plaintext greeting on 465
// will be disconnected immediately.
//
// Set the session deadline **when connecting**, not afterward: net/smtp.Client hides
// its underlying connection in an unexported field, so external code cannot access it.
// Once handed off, only a preconfigured deadline can enforce the timeout; this also
// covers a blocked handshake.
// Attach the same blockInternalDial guard used by HTTP channels. Without it, SMTP would
// be an SSRF gap: setting host to 169.254.169.254 or 127.0.0.1 would connect directly.
// A failed smtp.NewClient handshake includes the peer's response in the error, which
// delivery history would echo from last_error, creating a semi-blind read primitive.
// Timing differences between "connection refused" and a timeout can also probe ports.
// Dial-time enforcement is the final check and also covers DNS rebinding.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SMTP server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	return client, nil
}

// smtpStageError classifies an SMTP stage failure as retryable or permanent based on the response code.
//
// SMTP 4xx and 5xx responses have very different meanings:
//   - 4xx (450 greylisting, 451 local error, 452 insufficient storage) are **temporary**
//     rejections that should be retried later; greylisting is especially common on first delivery.
//   - 5xx (550 user does not exist, 553 invalid address) are permanent rejections and should not be retried.
//
// Treating all failures as permanent would make a server using greylisting mark **every**
// notification failed after the first attempt, even though this is exactly when automatic
// retries are most useful. The response code is the first three digits in the error text;
// if no code can be parsed, treat the error as retryable rather than ruling out a
// potentially transient failure.
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode extracts the leading three-digit response code from SMTP error text,
// returning 0 if none is found. net/smtp does not export a response-code field, so it
// must be parsed from text such as "450 4.7.1 ...".
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage assembles a complete RFC 5322 email.
//
// The body is Base64-encoded for two reasons: SMTP limits lines to 1000 bytes, and
// HTML bodies (especially digests) can have long lines; also, Base64 cannot produce
// lines beginning with ".", avoiding SMTP dot-stuffing.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// Non-ASCII subjects require RFC 2047 encoding to display correctly in email clients.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Email has no hard length limit, so do not truncate the body.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// Wrap Base64 at 76 characters, as required by RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
