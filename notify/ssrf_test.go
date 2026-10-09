package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related security measures:
//   1. Delivery URLs must not use the server to reach private networks/cloud metadata (SSRF).
//   2. URL-validation errors must not expose credentials in the address.
//
// Test setup: many package tests use httptest receivers on 127.0.0.1, which the guard
// blocks by default. TestMain therefore enables AllowLocalTargetsEnv, while each SSRF
// test below explicitly clears it to verify the **default-deny** behavior.

func TestMain(m *testing.M) {
	// Allow regular tests to connect to local mock receivers; SSRF tests temporarily clear this.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core SSRF assertion: by default, delivery
// to loopback addresses must be rejected at the **connection layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // Disable the opt-in to restore default behavior.
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("loopback delivery should be denied by default")
	}
	if hit {
		t.Fatal("request reached the local service; the guard did not work")
	}
	// The error should tell users how to allow this (a local SMTP relay is a valid setup).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("rejection should explain how to explicitly allow it: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the inverse: explicitly enabling local
// targets must work, or valid local Postfix/private relay deployments would be blocked.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work after explicitly allowing local targets: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // Cloud metadata endpoint; the main reason this function exists.
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // Unmap IPv4-mapped addresses before checking to prevent bypasses.
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private networks are **deliberately allowed**: self-hosted Mattermost/SMTP
	// relays are common. This assertion locks down the tradeoff; if private-network
	// checks are later added, this test forces an explicit decision rather than silently
	// breaking existing deployments.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private networks are common valid delivery targets)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers early validation feedback:
// literal private IPs should be rejected on save, not after the first delivery fails.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected during config validation", raw)
		}
	}
	// Public and private addresses still pass (private addresses are checked at dial time).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials covers a branch missed in the previous review.
//
// When url.Parse **fails**, it returns *url.Error, whose Error() includes the full
// original URL. The previous change redacted errors from http.Client.Do but missed
// this branch; the permanent-failure tests then added for file://, gopher://, and
// ftp:// actually parse successfully and exercise the scheme branch, so passing those
// tests did not prove this path safe.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // Invalid percent escape.
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // Nonnumeric port.
		"http://[::1?access_token=" + leakProbeToken,                  // Unmatched bracket.
	}
	for _, raw := range cases {
		// First confirm this input **actually** causes url.Parse to fail. Without this,
		// the test could unknowingly exercise a different branch (the previous false assurance).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should fail parsing; otherwise this test does not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm the channel-level wrapper does not expose the address either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("invalid address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault verifies the SMTP channel's dial guard.
//
// The email channel once used a bare net.Dialer, the only gap in SSRF protection:
// setting host to 169.254.169.254 or 127.0.0.1 connected directly. If smtp.NewClient's
// handshake failed, the peer's response was included in the error and echoed in
// delivery history via last_error, recreating the semi-blind read primitive already
// blocked in other channels. Timing differences between "connection refused" and
// timeout could also probe ports.
//
// TestMain enables AllowLocalTargetsEnv globally because many tests use local mock
// receivers. This test must clear it explicitly; otherwise it would pass whether or
// not the guard exists, which is why the gap went unnoticed.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // Disable the opt-in to restore default behavior.
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("email delivery to loopback should be denied by default")
	}
	// The connection must not be established: the guard blocks it in the Control hook,
	// so EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP session was established; the guard did not work")
	}
	// The error should explain how to allow this (a local Postfix relay is a valid setup).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("rejection should explain how to explicitly allow it: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired inverse: explicitly
// enabling local targets must allow delivery. Private-network/self-hosted SMTP relays
// and local relays are common deployments, so the guard must not block them wholesale.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("local SMTP delivery should work after explicitly allowing it: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO was not observed; the session was not established")
	}
}
