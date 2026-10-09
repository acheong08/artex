package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column, containing
// a channel's filtering criteria. All fields are optional; the default is no filtering,
// which is also the fallback for malformed config (see ParseFilter).
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical); empty means no threshold.
	MinSeverity string `json:"min_severity"`
	// Empty TaskIDs / AssetIDs means unrestricted; otherwise the event must intersect the list.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// Empty VulnClassInclude accepts all; otherwise vulnclass must match at least one keyword.
	// Matching any VulnClassExclude keyword excludes the event (exclusion takes precedence).
	// Matching is case-insensitive substring matching, avoiding silent failures caused by invalid regexes.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange controls whether the channel receives finding status-change events (realtime mode only).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses channel filter config.
//
// **Never returns an error.** Malformed filter config falls back to a zero-value Filter
// (no filtering, so everything matches). For a vulnerability notification system,
// **sending one extra notification is much better than silently dropping a critical
// finding**. Treating parse errors as "do not send" would make a seemingly configured
// channel send nothing, the worst failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure, f remains zero-valued, which means no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (empty means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks fields with **restricted values** in the filter config, for use when saving a channel.
//
// This must be enforced on write: Match treats an unknown threshold as `rank >= 0`,
// which is always true. A typo in min_severity (e.g. "hgih") would silently disable
// the filter and send everything. Although this follows the package's preference for
// extra notifications over dropped findings, users would believe severity filtering
// was active while every finding was sent, with no indication of the mistake. Reject
// this kind of silent degradation at the entry point.
//
// Validate is only for **write** paths. Reads still use ParseFilter's tolerant behavior
// so malformed values in existing data do not make the whole channel unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("invalid minimum severity %q; choose low / medium / high / critical, or leave blank for no limit", f.MinSeverity)
	}
	return nil
}

// Match determines whether an event should be delivered to a channel with this filter.
//
// **Never returns an error**, for the same reason as ParseFilter: treat internal
// anomalies as a match. Evaluation order: event type -> severity threshold -> task/
// asset scope -> finding-class keywords.
func Match(f Filter, s Snapshot) bool {
	// Only channels that explicitly enable status-change events receive them. Disabled
	// by default because most users expect "notifications" to mean new findings, not
	// a log of every status transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusions take precedence: any excluded keyword rejects the event even if it
	// also matches the include list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// Linear scans are adequate for these small sets (typically a few dozen manually
	// selected entries); building a map would cost more than it saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any keyword, case-insensitively.
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
