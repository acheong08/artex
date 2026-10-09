package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// This file verifies the invariant that **no** error text from channel implementations contains credentials.
//
// This is separate because the original channel tests covered only success and
// platform application errors, not transport failures. Transport errors (connection
// refused/DNS failure/timeout) are particularly risky: http.Client.Do returns
// *url.Error, which includes the **full URL**, and these providers embed credentials
// in it. Credentials could leak through four paths:
//
//	notification_deliveries.last_error -> stored in plaintext
//	GET /api/notify/deliveries response -> returned to browser, bypassing channel-config masking
//	server logs                         -> often forwarded and retained
//	test-delivery endpoint's 502 response -> shown directly in the frontend
//
// Therefore, rather than testing only one function, send a guaranteed-to-fail request
// through each channel and assert that its credential is absent from the error text.

// credentialCases covers all channel forms with credentials in the URL:
// DingTalk/WeCom in the query, Feishu in the final path segment, Telegram in the middle.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk access_token in query",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom key in query",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu hook ID in final path segment",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token in middle path segment",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk signing secret",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken is a sentinel value that cannot be a real credential, used to search error text.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials checks the core invariant.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// Guaranteed failure: nothing listens on 127.0.0.1:1, so the connection is refused.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "credential leak probe"}},
			})
			if err == nil {
				t.Fatal("unreachable address should return an error")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath covers permanent-failure
// paths: URL validation and platform application errors may also be returned externally
// and must not contain credentials.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// Invalid URL containing a credential, exercising validateHTTPURL / url.Parse.
		{"invalid DingTalk URL", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"invalid WeCom URL", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"invalid Feishu URL", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"invalid Telegram API URL", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"invalid generic Webhook URL", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("invalid config should return an error")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("error text leaked credential %q:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q, expected %q", in, got, want)
		}
		// The redacted result must not contain any path or query fragment from the original address.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("redacted result still contains path/query fragment %q: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// Never echo an unparseable input.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("unparseable input %q was echoed as %q", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL specifically checks *url.Error, the error type
// returned by http.Client.Do and the first point at which credentials could leak.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("host should remain for troubleshooting, got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("underlying cause should remain for troubleshooting, got %q", got)
	}
	// Keep Op too; whether the request was POST or GET is useful for troubleshooting.
	if !strings.Contains(got, "Post") {
		t.Errorf("operation name should remain, got %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback covers the fallback for addresses in custom
// errors that are not *url.Error (e.g. errors returned by redirect policy).
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("cross-host redirect rejected (a.example -> http://b.example/bot%s/send)", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("address should be replaced with a redacted form, got %q", got)
	}
	// Text without an address remains unchanged.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("text without an address should not change")
	}
}

// TestCrossHostRedirectRefused verifies that credentials in a URL are exposed by
// following cross-host redirects. The httptest servers listen on different ports on
// 127.0.0.1, so their hosts differ and form a cross-host redirect.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("cross-host redirect should be rejected")
	}
	if hit {
		t.Fatal("redirect target was accessed; credentials would have leaked")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed is the inverse: same-host redirects (e.g. adding a
// trailing slash) must remain usable or normal workflows would be blocked.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// Same host and port.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("same-host redirect should not be rejected: %v", err)
	}
}
