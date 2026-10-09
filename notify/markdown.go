package notify

import (
	"fmt"
	"strings"
)

// This file renders messages shared by Markdown-based channels (DingTalk and WeCom).
// Feishu uses card JSON, Telegram uses HTML, and email uses HTML; those are rendered
// by their respective adapters.

// maxAssetsShown is the maximum number of assets to list in a message. A finding may
// be tied to dozens of assets; listing all of them would bloat the message without
// adding value (no one will read more than three domains in IM).
const maxAssetsShown = 3

// maxSummaryRunes is the character limit for summaries. IM messages point readers to
// details; they are not the report itself, which remains on the platform.
const maxSummaryRunes = 120

// markdownReservedBytes reserves space for the header (digest summary, severity
// distribution, and possible truncation notice) and footer (platform link). Subtract
// this from the budget when packing whole items so the header and footer are not
// truncated; otherwise readers would not know which batch it is or whether items are missing.
const markdownReservedBytes = 320

// markdownEscape escapes Markdown metacharacters.
//
// This is necessary because finding titles, summaries, types, and asset display names
// all come from **untrusted sources**: titles and summaries are model output (based on
// target responses), while asset URLs come from scans and may contain target-controlled
// query strings. Without escaping, a finding titled
//
//	Login SQL injection\n[Urgent: click here to verify the account](http://attacker.tld)
//
// would render as a **clickable external link** in security engineers' DingTalk/
// Feishu messages. `![](http://attacker.tld/beacon)` would be fetched during rendering,
// signaling that the finding was viewed and exposing the reader's IP. Even benign
// injected bold text or blockquotes could push severe findings below the fold.
//
// The escape set covers characters that alter structure or create clickable elements
// in headings/links/emphasis/lists/quotes/strikethrough. Escape `\` first, or the
// backslashes added later will themselves be escaped.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText flattens untrusted text to one line and escapes it for Markdown bodies.
// Flattening is as important as escaping: newlines can create list items or blockquotes,
// which escaping does not prevent.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (IM title bar/card title) as **unescaped text**.
//
// It is deliberately not escaped because this title is shared by four render contexts:
// Markdown bodies, Telegram HTML, Feishu card plain_text, and generic Webhook JSON/email
// subjects. Each context has different escaping rules (Markdown escapes in HTML leave
// visible backslashes, and in JSON they corrupt data), so output handlers must escape
// it themselves; see writeItem / feishuItemLines / telegramEscape. Markdown escaping
// was once added here and caused visible backslashes like `\(1\)` in Telegram.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Finding summary · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Finding notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body and returns the **number of items actually written**.
//
// kept is the number of items actually delivered. The caller uses it to mark only the
// first kept items as delivered; items beyond the channel limit must remain for the
// next batch, not be marked successful. Otherwise truncation silently loses items:
// delivery history says everything was sent, with no indication that the rest was omitted.
//
// maxBytes<=0 means unlimited.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// Send an oversized single-item message anyway (final truncation is the fallback):
		// some information about a finding is better than sending nothing.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all in the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the digest header: time window, item count, and severity
// distribution. Recipients can decide whether to act immediately without opening the platform.
//
// items contains the items that **actually fit**; total is the intended batch size. If
// they differ, explicitly say how many items are in the next message; otherwise readers
// will think the header count is complete, with no indication that later items were omitted.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d new findings in the last %d minutes**", total, m.WindowMinutes)
	} else {
		fmt.Fprintf(&b, "**%d new findings**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(showing %d here; the remaining %d will be sent in the next message)", len(items), extra)
	}
	// Show the severity distribution so readers can quickly spot severe items. Count only
	// items **actually included in this message**, keeping the count consistent with the list.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders one finding.
//
// prefix is the sequence number in digest lists. single=true renders the full item
// (including summary and backlink); digests use one summary line per item, or a batch
// of 50 would become a long report.
//
// All external content (title/type/asset/summary) goes through markdownText for
// flattening and escaping. Backlinks are built from the admin-configured public_base_url,
// are trusted, and must remain clickable, so they are output as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: render one line, followed by compacted assets and summary.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Asset**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
