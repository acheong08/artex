package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is a key invariant: WeCom limits **bytes**, and multibyte characters can be
	// split by byte slicing, producing invalid UTF-8 that the platform rejects. Use
	// varied multibyte input with relatively prime lengths to exercise possible cut points.
	inputs := []string{
		"multibyte test content λ",
		"mixed café content",
		"aλbδcηdθe",
		"🔴🟠🟡🔵", // Four-byte emoji make incorrect cuts more obvious.
		strings.Repeat("vulnerability λ", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result is %d bytes, exceeding the limit", in, max, len(got))
			}
			// Content must not change when it is not truncated.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: content changed despite fitting the limit -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean unlimited")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean unlimited")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// If max is smaller than the ellipsis itself, appending it must not exceed the limit.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("result %q with max=1 has length %d, exceeding the limit", got, len(got))
	}
	// Normal truncation should include an ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected an ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// Preserve the difference from TruncateBytes: Telegram limits characters, and
	// byte-based truncation would leave far less of multibyte text.
	s := "αβγδεζηθικ"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 characters, got %d (%q)", n, got)
	}
	// The same string should be noticeably shorter when measured in bytes.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("byte-based truncation should not produce the same character count")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("First line\n\nSecond line\twith tabs   and spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("expected all whitespace to be collapsed, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("consecutive spaces should not remain, got %q", got)
	}
	// Truncated output must remain readable and valid.
	got = OneLine("alpha beta gamma delta epsilon", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Truncating HTML directly could leave a fragment like `<a href="htt`, causing
	// the platform to reject the whole message.
	s := `<b>Title</b>body body body<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result is %d characters, exceeding the limit", max, n)
		}
		// The end must not contain an unclosed tag (a final '<' without '>').
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: trailing tag was cut off -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should return an empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("all items should be listed when under the limit, got %q", got)
	}
	// When over the limit, include the total so readers know how many assets are omitted.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "and 5 more") {
		t.Fatalf("expected to indicate 5 total items, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("empty severity has rank 0 and should be rejected by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("empty threshold should allow the value")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("status label mapping is incorrect: %q", got)
	}
	// Unknown statuses are returned as-is; do not invent labels.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("unknown status should be returned as-is, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers an overlooked case: truncation must avoid
// cutting both HTML tags and entities in half.
//
// A parser that accepts only complete entities might reject the **entire** message if
// `&amp;` is truncated to `&amp`; oversized digests are common enough that this is costly.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The end must not contain an entity fragment with '&' but no corresponding ';'.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: trailing entity fragment remains %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity found", max)
		}
	}
}
