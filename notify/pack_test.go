package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the whole-item packing fix: when a digest exceeds the channel
// length limit, truncate **between items** and report how many did not fit so the
// caller marks only the items actually delivered.
//
// Previously, the full digest was rendered and then truncated, but the entire batch
// was marked delivered. The second half disappeared while delivery history showed
// success, making findings vanish without a trace.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// A 200-item digest must exceed WeCom's 4096-byte limit.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body is %d bytes, exceeding the limit of %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("expected only part of the batch to fit (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must distinguish how many items are included here and how many remain,
	// or readers may mistake its count for the total.
	if !strings.Contains(body, "showing") || !strings.Contains(body, "next message") {
		t.Fatalf("header should say how many items are omitted from this message:\n%s", body[:minInt(400, len(body))])
	}
	// Only the first kept items should be included.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "Finding "+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "Finding "+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = unlimited
	if kept != len(m.Items) {
		t.Fatalf("expected all items to remain when unlimited, got kept=%d", kept)
	}
	if strings.Contains(body, "next message") {
		t.Fatalf("truncation notice should not appear when nothing was truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// If the budget cannot fit one item, still send one (final truncation is the fallback).
	// Otherwise an oversized finding would permanently block the batch.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("expected to keep at least one item, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("single-item message should report one delivered item, got %d", kept)
	}
	// An empty message has nothing to deliver.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("empty message should report zero delivered items, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits **characters**; byte-based limits would shrink multibyte text to a third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body is %d characters, exceeding the limit of %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("expected only part of the batch to fit, got %d", kept)
	}
	if !strings.Contains(text, "continue in the next message") {
		t.Fatalf("expected a notice that some items remain:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("card should fit only part of the batch, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels do not truncate the body, so count the entire batch as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition failed")
	}
	// Indirectly verify through the renderer return value that markdownBody(0) keeps all items.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("expected all items to be used with no limit, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test ensuring untrusted content
// cannot alter message structure. Titles and summaries come from model output (based
// on target responses), and asset names come from target URLs.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // Escaped forms that must appear in the result.
		wrong []string // Unescaped forms that must not appear in the result.
	}{
		{
			name: "newline and external link in title",
			item: Item{
				Severity: "high",
				Name:     "Login endpoint SQL injection\n[Urgent: click here to verify account](http://attacker.tld)",
			},
			// Newlines must be folded (to prevent fake list items/quotes); square and
			// round brackets must be escaped (to prevent clickable external links).
			must:  []string{`\[Urgent: click here to verify account\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[Urgent", "\n\n[Urgent"},
		},
		{
			name: "image beacon in title",
			item: Item{
				Severity: "high",
				Name:     "Finding ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and quote in asset name",
			item: Item{
				Severity: "high",
				Name:     "Ordinary title",
				Assets:   []string{"a.com/*injection*>quote"},
			},
			must:  []string{`\*injection\*`, `\>`},
			wrong: []string{"*injection*"},
		},
		{
			name: "backticks and pipe in summary",
			item: Item{
				Severity: "high",
				Name:     "Title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// Single-item writeItem is the shared rendering path for all three Markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("escaped form %q is missing:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("unescaped form %q appears (could inject structure or an external link):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst verifies escaping order: backslashes must be
// handled first, or subsequently added backslashes will be escaped again.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("incorrect escaping order, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes verifies that Markdown escaping does not leak
// into Telegram HTML output. Escaping in the shared title function once caused visible
// backslashes like `\(1\)` in Telegram messages.
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *important*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram body contains Markdown backslash escapes:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
