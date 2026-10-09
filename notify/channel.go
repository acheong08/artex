package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel adapts a notification channel. Implementations must be **stateless**:
// one instance is shared concurrently by multiple channel configs, with credentials
// always passed through cfg.
type Channel interface {
	// Kind returns the channel type identifier, which must match a registry key.
	Kind() string
	// Validate checks required fields and formats when saving config. Its errors are
	// shown directly to the user, so they should name the missing field rather than
	// say only "invalid config."
	Validate(cfg map[string]any) error
	// Send delivers a message and returns the **number of items actually delivered**
	// and any error.
	//
	// The count matters because platforms limit message length, so a digest that cannot
	// fit the whole batch is truncated. If the caller marks the entire batch delivered,
	// truncated items disappear: they are absent from the message and delivery history
	// still shows success. Returning kept lets the caller mark only the first kept items,
	// leaving the rest for the next batch.
	//
	// An error indicates delivery failed; *PermanentError means it should not be retried.
	// On failure, kept is meaningless and callers should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the channel's officially recommended deliveries per
	// minute, used as the default rate limit for new channel instances. Zero means no
	// known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the config keys that contain credentials. Their values are
	// masked in API responses; when a masked value is received on update, the stored
	// value is preserved. Only the implementation knows which fields are credentials
	// (the entire WeCom webhook URL is a credential, while only the secret is for
	// DingTalk), so the channel must provide this information.
	SecretKeys() []string
	// DestinationKeys returns config keys that determine where messages are sent.
	//
	// This is security-sensitive, like SecretKeys: the destination and credentials are
	// separate fields. If someone can change only the destination while preserving
	// credentials, anyone able to edit channel config could send stored credentials to
	// a server they control, defeating config masking. See PrepareConfigUpdate.
	DestinationKeys() []string
}

// registry is the channel registry. Explicit entries are used instead of init()-based
// self-registration so all supported channels are visible in one place and omissions
// are caught during development rather than through runtime side effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get returns the channel implementation for a type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types in lexicographic order (for stable UI dropdowns).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried, such as invalid
// credentials, a rejected destination, or an invalid request body. Retries only help
// for transient failures (network glitches, rate limits, or peer 5xx responses);
// repeatedly backing off on a permanent failure will not help and obscures the real
// error in the retry log.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err is nil, allowing
// the concise form `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the error chain contains a permanent-failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- Config-reading helpers ----
//
// Channel config comes from a database JSONB column and is decoded by encoding/json
// into map[string]any; numbers become float64 and arrays become []any. These helpers
// normalize the conversions and tolerate type variations caused by UI input (e.g. a
// port entered as a string).

// cfgString returns a string config value with surrounding whitespace removed, which
// is often included when copying and pasting from web forms.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt returns an integer config value, accepting float64 (the JSON default) and strings.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool returns a boolean config value, accepting the strings "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings returns an array of string config values, trimming whitespace and dropping empty values.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accept a single string for form submissions with one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap returns a string-map config value (e.g. custom HTTP headers), trimming keys
// and values and dropping empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
