package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements DingTalk custom bots.
//
// Platform characteristics that determine implementation choices:
//   - Each bot is rate-limited to 20 messages per minute; excess messages are silently
//     dropped (HTTP may still be 200), so rate limiting must be client-side (see DefaultRatePerMin).
//   - Security settings are one of: signature, custom keywords, or IP allowlist.
//     Signatures are the only option independent of message content, so only signatures
//     (or no setting, using a bare webhook) are supported.
//   - Both success and failure return HTTP 200; the body errcode distinguishes them.
//     Ignoring errcode would incorrectly mark failed deliveries as successful.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk webhook URLs contain access_token and are credentials themselves, so mask the entire URL.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The DingTalk webhook URL is the destination; changing it requires explicitly resubmitting the signature secret.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook URL is required")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

// Send delivers one message. Use ActionCard (with a button) for a single item with a backlink; otherwise use Markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk Markdown has no documented byte limit, but cap it to protect against unusually large evidence fields.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides application errors in HTTP 200 responses.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 means signature validation failed; 310000 means a keyword did not match.
		// Both are config errors and will not recover on retry.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL adds timestamp and sign parameters to a webhook URL using the official signing scheme.
//
// Scheme: string to sign = timestamp + "\n" + secret; the HMAC-SHA256 **key is also
// secret**. Base64-encode and URL-encode the result. timestamp is in milliseconds.
// Return the URL unchanged for bots without signature validation enabled (empty secret).
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Do not expose err: url.Parse errors include the full address, including access_token.
		return "", fmt.Errorf("failed to parse Webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks address validity and supported protocols, and checks literal IP targets for internal addresses.
//
// Two details matter:
//
//  1. **Errors must be redacted.** url.Parse returns *url.Error, whose Error() includes
//     the **full original address**. Addresses for these providers embed credentials
//     (DingTalk access_token, WeCom key, Telegram bot token, Feishu hook ID). Returning
//     err directly once leaked credentials through the "invalid address" error into
//     test-endpoint 400 responses, stored delivery last_error, server logs, and delivery history.
//
//  2. **Check literal IPs for internal addresses immediately**; leave hostnames to the
//     dial stage (blockInternalDial is the final enforcement point and also handles
//     DNS rebinding). Checking here gives users feedback while saving config instead of
//     waiting for the first delivery to fail.
//
// Restricting protocols is defense in depth: schemes such as file:// or gopher:// can
// cause unexpected http.Client behavior. Scheme checks already block them, but there
// is no reason to allow these protocols.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("could not parse address (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https are supported; received %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("hostname is required")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("delivery to loopback/link-local address %s is blocked (to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
