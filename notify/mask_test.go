package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("masked value leaked the full credential: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("masked value should not expose the address body: %q", got)
	}
	// Preserve the last six characters so users can identify the bot.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("expected the final six characters to remain as an identifying hint: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("masked value should be recognized by IsMasked: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// Exposing the final six characters of a short credential would reveal it entirely.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("credential of length %d should not have a suffix hint, got %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("masked value contains the original: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s should be masked, got %q", k, s)
		}
	}
	// Non-credential fields must remain unchanged so the UI can display them.
	if masked["port"] != float64(587) {
		t.Errorf("non-credential field port should not change: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// For an unknown channel type, show an empty config rather than returning potentially credential-bearing raw data.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("unknown channel type should return an empty config, got %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// Masking is for display and must not change stored values.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig mutated its input, which could overwrite the actual credential with its masked value")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// The user changed only method; the browser submits masked values plus the new method.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("masked fields should preserve stored values, got %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("updated field should take effect, got %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("empty string should clear the field, got %v", got)
	}
	// Unmentioned fields are preserved (partial-update semantics).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("unmentioned field should be preserved, got %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("unmentioned fields should be preserved, got %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("mentioned field should be updated, got %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap verifies this package's most important
// security invariant: **changing the destination must not carry old credentials over**.
//
// These cases use attack-shaped input (change only the address and omit credentials),
// not merely valid defensive input; testing only the latter could pass even if the
// defense did not work.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing is the credential key expected to be identified as missing.
		wantMissing string
	}{
		{
			name: "generic Webhook changes URL while reusing Authorization header",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram changes base_url and would send Bot Token to a custom endpoint",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "email changes SMTP host and would expose password",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "email TLS change requires explicit password resubmission",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// A masked value means "reuse the old credential" and must also be rejected when changing the destination.
			name:        "masked credential submitted with new destination",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk changes Webhook and would reuse signing key",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("changing the destination without resubmitting credentials should be rejected; got config %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("expected a dedicated error type so the API can provide actionable guidance, got %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected missing credential key %q to be identified, got %v", tc.wantMissing, target.Missing)
			}
			// The error should tell the operator how to fix the problem.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("error should mention %q: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits checks the inverse case: legitimate
// edits must not be blocked, or users may bypass or remove the protection as too cumbersome.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "change only the name (config is returned unchanged)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "change only the request method; destination and credentials are unchanged",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "change destination and **also** provide new credentials",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "change destination and explicitly clear credentials",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram changes chat_id (not a destination)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "email changes recipient (not a destination)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("legitimate edit was incorrectly blocked: %v", err)
			}
			if merged == nil {
				t.Fatal("expected a merged result")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance covers an easy-to-misjudge detail: the
// frontend submits ports as JSON numbers (float64), but values may have different
// types after decoding (e.g. int vs float64). Comparing with == could mistake an
// unchanged value for a change and prompt users who only changed the name to resubmit
// their password; false alarms undermine trust in this protection.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// Submit the same port as an int.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("identical port values (with different types) should not count as a destination change: %v", err)
	}
	// A genuinely different port must be blocked.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("port change should be blocked")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination covers an optional
// destination field: an empty Telegram base_url means to use the official API address.
//
// This previously made channel saves fail permanently starting with the second save:
//
//	Creation stores base_url:"" (the create path stores submitted config directly, without MergeConfig).
//	-> First save: MergeConfig treats the empty string as an explicit clear and deletes the key.
//	-> Second save: incoming is still "", but the key is absent from stored config, so the destination appears to have changed.
//	-> bot_token is masked in the response, resulting in 400 "Destination changed; resubmit credentials."
//
// The user changed nothing but could no longer save without pasting the Bot Token again.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// Frontend buildConfig() submits every defined field for this channel: masked values
	// for credentials and empty strings for empty fields. Reproduce its complete output
	// here rather than submitting only changed keys.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// First save: only the channel name changed, and config is returned unchanged.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("first save was incorrectly blocked: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("precondition changed: MergeConfig should delete the empty value; this test covers the subsequent save after the key disappears")
	}

	// Second save: submitted content is identical; the user changed nothing.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("second save was incorrectly blocked although the user changed nothing: %v", err)
	}
	// Third save: confirm this is consistently saveable, not just a one-time success.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("third save was incorrectly blocked: %v", err)
	}
	// The credential must be preserved throughout and not cleared along with empty strings.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token should retain its original value, got %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges is paired with the
// previous test: treating an empty string and a missing key as equivalent **must not**
// allow a real destination change. Both directions can expose credentials because
// Telegram's Bot Token is in the URL path; changing base_url sends it to the new address.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// Direction one: change from empty (official address) to a self-hosted address.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("changing from the official address to a self-hosted address must require resubmitting the Token")
	}

	// Direction two: clearing a self-hosted address (switching back to the official API) is also a destination change.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("clearing a self-hosted address (switching back to the official API) is a destination change and must require resubmitting the Token")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// As with SecretKeys, if a channel does not declare destination keys, PrepareConfigUpdate cannot protect them.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("channel %s has not declared destination keys, so credential protection on destination changes cannot work", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("channel %s has not declared credential keys", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// The compiler requires every channel to implement SecretKeys; also verify that no
	// channel leaves masking unimplemented. An empty slice means credentials are echoed
	// in plaintext to the browser.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("channel %s has no expected masking behavior registered in this test", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("channel %s declares no credential fields, so its config will be echoed in plaintext", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer covers a gap found during review:
// if a mask sentinel is placed in a **non-string** structure (e.g. webhook.headers),
// MergeConfig recognizes masks only as prefixed strings, so the literal "__masked__"
// could be stored as the real header value, silently breaking authentication.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// Include a mask sentinel inside an object.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("a mask sentinel inside an object should be rejected (otherwise the literal would be stored)")
	}
	// A complete submission with a real new value should still be accepted.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("a valid new header should not be blocked: %v", err)
	}
}
