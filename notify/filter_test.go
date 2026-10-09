package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// Malformed JSON, empty input, and fields of the wrong type must all fall back to
	// the zero-value Filter (no filtering). This implements the "extra notifications
	// are better than missed findings" rule: a typo must not silently drop critical alerts.
	cases := []struct {
		name string
		raw  string
	}{
		{"empty input", ""},
		{"invalid JSON", `{not json`},
		{"truncated JSON", `{"min_severity":`},
		{"type mismatch", `{"min_severity": 123, "task_ids": "abc"}`},
		{"top-level array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("malformed config should fall back to a zero-value Filter, got %+v", f)
			}
			// A zero-value Filter must match any event.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("zero-value Filter should match all events")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// Unknown severity has rank 0 and should be rejected by any nonempty threshold.
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: expected %v, got %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL injection",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"empty scope is unrestricted", Filter{}, true},
		{"task matches", Filter{TaskIDs: []int64{7}}, true},
		{"task does not match", Filter{TaskIDs: []int64{8}}, false},
		{"multiple tasks include a match", Filter{TaskIDs: []int64{8, 7}}, true},
		{"assets intersect", Filter{AssetIDs: []int64{20, 99}}, true},
		{"assets do not intersect", Filter{AssetIDs: []int64{99}}, false},
		{"task and asset both match", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"task matches but asset does not", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"empty include accepts all", Filter{}, "arbitrary type", true},
		{"include matches", Filter{VulnClassInclude: []string{"SQL"}}, "SQL injection", true},
		{"include does not match", Filter{VulnClassInclude: []string{"command execution"}}, "SQL injection", false},
		{"any include keyword matches", Filter{VulnClassInclude: []string{"command execution", "SQL"}}, "SQL injection", true},
		{"case-insensitive matching", Filter{VulnClassInclude: []string{"sql"}}, "SQL injection", true},
		{"exclude match rejects", Filter{VulnClassExclude: []string{"information disclosure"}}, "information disclosure", false},
		{"nonmatching exclude allows", Filter{VulnClassExclude: []string{"information disclosure"}}, "SQL injection", true},
		// Exclusion takes precedence: a match is rejected even if it also matches an inclusion.
		{"exclude takes precedence over include", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"injection"},
		}, "SQL injection", false},
		// Whitespace-only keywords must be ignored; otherwise they match every string containing spaces.
		{"whitespace keywords are ignored", Filter{VulnClassInclude: []string{"", "  "}}, "SQL injection", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// Off by default: most users expect finding notifications to mean new findings, not every status transition.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("status-change events should be skipped unless enabled")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("status-change events should match when on_status_change is enabled")
	}
	// Finding-created events are not affected by on_status_change.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("finding-created events should not depend on on_status_change")
	}
}
