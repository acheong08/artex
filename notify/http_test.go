package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// This file covers doJSON's HTTP-layer error classification.
//
// This deserves separate tests: channel adapters handle platform-specific application
// error codes (DingTalk errcode, Feishu code, Telegram ok field), while doJSON handles
// **HTTP-layer** classification. These are independent safeguards. Without this layer,
// a 503 from a proxy gateway would be treated as permanent and not retried, while a 403
// would be retried three times unnecessarily.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 success is not an error", 200, false},
		{"429 rate limit is retryable", 429, false},
		{"408 request timeout is retryable", 408, false},
		{"500 server error is retryable", 500, false},
		{"502 gateway error is retryable", 502, false},
		{"503 service unavailable is retryable", 503, false},
		{"400 invalid parameters is permanent", 400, true},
		{"401 authentication failure is permanent", 401, true},
		{"403 forbidden is permanent", 403, true},
		{"404 address not found is permanent", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx should not return an error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("non-2xx should return an error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("incorrect permanent classification for HTTP %d: expected %v, got %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// Include the status code in errors so users can distinguish misconfiguration
			// from a peer failure. Assert the number rather than Go's StatusText so the
			// assertion is language-independent and stable.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("error should include HTTP status %d, got %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet verifies that peer error details are included;
// otherwise users know only that delivery failed, not why the peer rejected it.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("error should include the peer's explanation, got %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded constrains the snippet format. Peer
// responses are stored verbatim in last_error and shown in the frontend table, where
// multiline or oversized text would break the layout and payload.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// Response content with newlines, tabs, and 5,000 characters.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("error should be a single line, got %q", msg)
	}
	// The snippet is capped at 200 characters plus a fixed prefix, well below the original response.
	if len(msg) > 400 {
		t.Errorf("error is too long (%d bytes); snippet should truncate it: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse verifies the read limit: if a peer returns an
// unusually large response, do not read all of it into memory (each last_error is stored).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > 400 {
		t.Errorf("oversized response should be read with a limit and truncated; error length is %d", len(err.Error()))
	}
}
