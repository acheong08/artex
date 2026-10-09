package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, preserving valid UTF-8 and whole characters.
//
// Truncate at character boundaries: WeCom group-bot Markdown has a 4096-**byte**
// hard limit (not characters), and multibyte characters may span three bytes. Slicing
// directly at a byte offset can split a character and produce invalid UTF-8, causing
// the platform to reject the message or display replacement characters. Start at the
// budget boundary and move back to the beginning of the most recent rune
// (utf8.RuneStart recognizes continuation bytes 0b10xxxxxx).
//
// max<=0 means unlimited. Add an ellipsis after truncation unless max is too small for it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis: omit it and truncate directly to avoid exceeding max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine compresses multiline text to one line by folding whitespace, then truncates
// by character count. Used for IM title lines, where newlines in summaries would break
// table/title layout. max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), adding an ellipsis
// when needed. max<=0 means unlimited.
//
// The distinction follows platform limits: WeCom counts bytes, while Telegram counts
// characters. Using the wrong unit will not return an error but will truncate far
// more than expected (a three-byte character means a 4096-byte limit leaves only about
// 1,365 characters), so keep both functions and choose per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count without cutting tags in half.
//
// Direct character-based truncation could leave fragments like `<a href="htt`, causing
// the platform parser to reject the entire message or consume later text as an
// attribute value. Truncate by characters first, then move back before any unmatched
// trailing `<`.
//
// Do not balance tags (e.g. add </b>): Telegram's HTML parser closes unclosed tags
// automatically, while implementing balancing would need to handle quotes in
// attributes, comments, and self-closing tags for little benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with '<' (the final '<' has no following '>'), move before it.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// Also move back if the tail contains a truncated HTML entity (e.g. `&amp;` cut to
	// `&amp`). An entity fragment may cause parsers that require entities to be valid to
	// reject the **entire message**; oversized digests are common enough that this is not
	// worth risking.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount calculates how many complete items fit within the budget, for packing digests by item.
//
// Pack complete items instead of rendering everything and truncating: truncation would
// make later items disappear while their delivery records still say they were sent.
// They would be absent from both the message and delivery history. With whole-item
// packing, items that do not fit remain in the database for the next batch, and kept
// tells callers how many were actually delivered in this message.
//
// Parameters: maxSize<=0 means unlimited; reserve is space held for the header/footer;
// size measures content (platform units differ: WeCom/DingTalk use bytes, Telegram uses
// characters; the wrong unit will over-truncate multibyte text); render produces the
// actual text for item idx, whose length varies with content and cannot be estimated.
//
// Return at least 1 if any items exist. Even an extremely long single item should be
// sent and left to the caller's final truncation; otherwise it would permanently block the batch.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize name the two measurement units used by packItemCount, making the
// unit obvious at call sites instead of hiding it in an anonymous func(s string) int.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders assets on one display line, omitting extras and showing the total
// when there are more than limit. A finding may be tied to dozens of assets, and listing
// all of them would bloat the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " and " + itoa(len(assets)) + " more"
}

// itoa is a short alias for strconv.Itoa, used only to format display text and avoid importing strconv elsewhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
