package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard byte limit (not characters) for WeCom group-bot Markdown content.
// It is the tightest limit among all six channels and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements WeCom group bots.
//
// Platform characteristics:
//   - Authentication uses a key in the URL; signing is unsupported, so the webhook URL
//     itself is the complete credential.
//   - Markdown content is limited to 4096 **bytes**; oversized messages are rejected
//     rather than truncated. Multibyte text means only about 1,300 characters fit, so
//     truncation must be handled client-side.
//   - Rate limited to 20 messages/minute; enforce this client-side too.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only one credential (the key in the webhook URL) and does not support
// signatures, so the entire URL is the credential; no other fields need masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only one webhook field, which is both destination and credential, so there
// is no credential left over after changing the address.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook URL is required")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A digest can be long (50 items, one line each, plus a prefix) and easily exceed
	// 4096 bytes. Truncate here rather than relying on platform rejection: rejection
	// drops the whole batch, while truncation delivers at least the first items.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 means the API call limit was exceeded. The platform's rate-limit window
		// rolls, so retrying with backoff can help; classify it as retryable. Reaching
		// this point means client rate_per_min is too aggressive. Retries are a fallback;
		// the real fix is to lower the channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limited the request %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 means the webhook key is invalid: a permanent failure that will not recover on retry.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
