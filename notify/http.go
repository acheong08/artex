package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets controls whether notifications may be sent to loopback/link-local addresses.
//
// Denied by default. IM bots and public mail servers do not use these addresses, but
// they can reach sensitive services: another service's admin port on the same host or
// cloud metadata endpoints (169.254.169.254, which can expose instance credentials).
// Although administrators configure delivery addresses, an admin session exploited
// via XSS/CSRF or another user sharing the same JWT could change the config and read
// the response. doJSON stores the first 200 bytes of 4xx/5xx response bodies in
// last_error, and the delivery-history API returns it, creating a semi-blind read
// primitive.
//
// However, a local SMTP relay (e.g. postfix on 127.0.0.1:25) is common for self-hosted
// email, so rejecting all local targets would block a valid setup. Provide an explicit
// opt-in rather than allowing it by default: set ARTEX_NOTIFY_ALLOW_LOCAL=1.
//
// Export AllowLocalTargetsEnv so tests can enable it explicitly. Tests in this and the
// server package use httptest receivers on 127.0.0.1, which the guard would otherwise block.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP is in a range denied by default for delivery.
//
// Deny only loopback, link-local (including cloud metadata 169.254.169.254), unspecified,
// and multicast addresses. **Do not deny** RFC1918 private networks: self-hosted
// Mattermost/SMTP relays on private networks are common legitimate setups, and blocking
// them would make this unusable in real deployments. This tradeoff is deliberate:
// block genuinely sensitive targets without breaking normal deployments.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Unmap IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1) before checking, or the check can be bypassed.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is an http.Transport dialer's Control hook that checks the target
// address **when the connection is established**.
//
// The dial stage is the final enforcement point, unlike checking only when config is
// saved. It also covers two ways to bypass config validation: DNS rebinding (resolving
// to a public IP during validation and a private one at connection time) and redirects
// (cross-host redirects are rejected, but same-host redirects can still change paths).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot resolve target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("delivery to loopback/link-local address %s is blocked (to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds only a dial guard to the default Transport. Clone preserves
// all default tuning (connection pools, HTTP/2, timeouts, proxies, etc.) and avoids
// changing unrelated behavior.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is shared by all channel deliveries.
//
// Deliberately **do not** reuse the project's global outbound proxy (server-side
// GlobalProxy): it is for pentest target traffic and is often an unstable tunnel.
// Notification availability should not depend on target-network instability. Direct
// connections are sufficient for IM delivery. The timeout is 15 seconds; a slower
// peer is effectively already failing.
//
// Reject cross-host redirects: deliveries use fixed endpoints and should not redirect
// to another host. Credentials for several providers (DingTalk access_token, WeCom key,
// Telegram bot token) are **in the URL**, so following a cross-host redirect would
// expose credentials to the redirect target. Same-host redirects (e.g. adding a
// trailing slash) remain allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("cross-host redirect blocked (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit caps response-body reads. A misbehaving peer may return huge content,
// but delivery history only needs the status code and a short error description.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns its response body (capped in size).
//
// A nil payload sends an empty body (for GET or platforms that do not require a body).
// Header key/value pairs are added as-is for generic Webhook custom headers.
//
// Error classification is this function's core responsibility: network failures and
// 5xx/408/429 are retryable; other 4xx responses are permanent failures. Retrying a
// 403 only writes the same error to the log three times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// Serialization failure is a local bug (wrong config-field type); retrying will not help.
			return nil, Permanent(fmt.Errorf("failed to construct request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Invalid URL, likely due to a typo in the configured address; this is permanent.
		// Do not expose err because url.Parse errors include the full address.
		return nil, Permanent(fmt.Errorf("invalid request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, or timeout: usually transient, so retry with backoff.
		//
		// Redact errors before returning them. http.Client.Do returns *url.Error, whose
		// Error() is `Op "full URL": underlying error`; credentials for these providers
		// are **in the URL** (DingTalk access_token, WeCom key, Feishu hook ID, Telegram
		// /bot<token>/). Without redaction, credentials could leak through four paths:
		// notification_deliveries.last_error (stored in plaintext), delivery-history API
		// responses (**bypassing channel-config masking**), server logs, and the 502 text
		// returned to the frontend by the test-delivery endpoint.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limit) and 408 (timeout) are retryable; other 4xx responses indicate
	// config or permission issues and are not worth retrying.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("remote service rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("remote service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("remote service rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses a response body into one short line for error messages. Responses
// may contain newlines and excessive whitespace, which would break delivery-history layout.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget reduces a delivery URL to "scheme://host/..." for error messages.
//
// This is the package's only URL-redaction policy and is deliberately **aggressive**:
// discard everything except the scheme and host. There is no universal, safe way to
// determine which part of a URL contains credentials:
//
//	DingTalk   credential in query       /robot/send?access_token=xxx
//	WeCom      credential in query       /cgi-bin/webhook/send?key=xxx
//	Feishu     credential in **last path segment** /open-apis/bot/v2/hook/<hook_id>
//	Telegram   credential in **middle path segment** /bot<token>/sendMessage
//
// Keeping only selected pieces would require per-channel exceptions, and missing one
// would leak credentials. Keeping the host is enough to diagnose DNS, connection, and
// certificate failures; the masked suffix in channel config identifies the bot.
//
// On parse failure, return a fixed placeholder; never echo the original string.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address could not be parsed)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError removes the URL from a transport error, retaining only the cause.
//
// *url.Error has the structure {Op, URL, Err}, and Error() includes the URL.
// Explicitly extract Err rather than calling Error() and replacing text afterward;
// this is more reliable because replacements can miss URL-encoded/escaped variants.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Non-*url.Error values (e.g. errors from redirect policy) may also contain URLs; redact them too.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces HTTP(S) URLs in a string with their redacted form.
//
// This is a fallback for errors without structured fields (redirect-policy errors,
// custom errors from third-party libraries). It recognizes only http/https prefixes
// and splits on whitespace and quotes, which are not valid URL characters here.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
