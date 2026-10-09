package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Operational constraints (highest priority; override all exploration/expansion heuristics below. Before creating each intent or taking any action, check for violations; do not proceed if a constraint would be violated.)]:")
	if len(allow) > 0 {
		b.WriteString("\nAllowed actions:\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\nProhibited actions:\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n(Discovering a new target, port, or host outside the constraints does not grant authorization. Unless it falls within the allowed scope above, record it as an out-of-scope fact and skip it; do not derive intents or take actions for it.)")
	return b.String()
}
