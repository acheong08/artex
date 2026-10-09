// Package notify implements notification-channel adapters for IM/email finding alerts.
//
// Architecture: this is a **leaf package** that depends only on the standard library.
// It knows nothing about the database or server. Channel config is passed as
// map[string]any (the notification_channels.config JSONB column), and content to
// deliver is passed as Message. This makes error-prone logic such as signature
// calculation, UTF-8 truncation, and filter matching testable without PostgreSQL;
// the host only needs to orchestrate delivery in the server layer.
//
// Concurrency contract: Channel implementations must be **stateless**. The same
// Channel instance is shared concurrently across multiple channel configs (including
// multiple bot instances of the same type). Credentials must always come from cfg;
// do not cache webhook URLs or similar values in implementation fields.
package notify

// Channel type identifiers. These are the allowed notification_channels.kind values,
// validated against a server-side allowlist (as with findings.status; no DB CHECK is
// used to make adding channels easier).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (including Lark) custom bot
	KindWeCom    = "wecom"    // WeCom group bot
	KindWebhook  = "webhook"  // Generic Webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback kind when config does not specify one.
const InitKind = KindDingTalk

// severityRank maps finding severities to comparable ranks. Unknown severities return
// 0, so any min_severity excludes them; when in doubt, do not send, avoiding noisy false positives.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the rank of a severity; unknown values return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the severity label with an emoji, for message titles and card colors.
// Unknown severities are returned as-is.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel returns a readable label for a finding status, for status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk accepted"
	default:
		return status
	}
}

// AtLeast reports whether severity meets the min threshold. An empty min means no
// threshold, so every value passes. Unknown severity ranks are 0 and are rejected by
// any non-empty min (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
