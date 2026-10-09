package notify

import (
	"net/url"
	"testing"
	"time"
)

// Signature reference values were independently calculated with OpenSSL rather than
// this package's implementation; otherwise this would only prove the code had not
// changed, not that the algorithm was correct.
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("signing failed: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("generated address is not parseable: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("signature mismatch\nexpected %s\ngot %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("timestamp should be included unchanged in milliseconds, got %q", q.Get("timestamp"))
	}
	// Existing query parameters (access_token) must not be overwritten by signing.
	if q.Get("access_token") != "tok" {
		t.Errorf("existing query parameter was lost, got %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("signature mismatch\nexpected %s\ngot %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer verifies the two distinct algorithms and their reversed
// parameter order (DingTalk key=secret, Feishu key=string to sign). Copying one from
// the other would fail validation; this test prevents a future refactor from merging them.
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk and Feishu signatures are identical; one algorithm is implemented incorrectly")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// A bot without signing enabled must not gain timestamp/sign parameters.
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("address should remain unchanged without a secret, got %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q should be accepted: %v", s, err)
		}
	}
	// Schemes such as file:// should not be allowed; http.Client behavior for them is out of scope.
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}
