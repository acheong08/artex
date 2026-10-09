package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**Finding ID conventions**: finding_id is the ID of an independent finding record; finding_node_id is the ID of an exploration node. The id returned by list_findings / list_task_findings / node_detail / get_task_node_detail remains an exploration-node ID; read the independent finding_id from the same response. get_finding_traffic / bind_finding_traffic use the independent finding_id. The legacy update_finding_report finding_id parameter still expects finding_node_id. Do not use the number on the first line of report_finding output with evidence tools, and do not guess another number if an ID is rejected."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\nBy default, the Reporter Agent verifies and binds traffic before writing the report. In evidence, retain verification commands, key output, and any existing real traffic IDs with their purpose so the Reporter Agent can cross-check them against execution traces; no extra traffic lookup is needed just for binding. Immediate explicit binding is also supported: traffic_refs or evidence_hint_id can supply verified references. The latter reads structured references from a hint in this task; if any reference is invalid, the entire report_finding call fails. These optional parameters are unnecessary for TCP or when no traffic was captured. The response's finding_id identifies the independent record; finding_node_id identifies the exploration node."
		case "add_hint", "add_task_hint":
			note = "\nWhen handing off a confirmed finding, retain verified traffic IDs, roles, notes, and order in traffic_refs on the corresponding hint (at the top level for a single hint, or on the relevant hints item for a batch), and explain in text which specific finding the traffic supports. Do not hand off only text while discarding existing traffic references. Unverified candidates must not be passed as evidence."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**Traffic evidence handoff (optional)**: By default, the Reporter Agent binds traffic after a finding is recorded and before writing its report. The reporter should retain verification commands, key output, and existing real traffic IDs with their purpose in evidence, and include intent_id in the task so the Reporter Agent can trace them; no extra traffic lookup is needed just for binding. Auto / Planner must not discard references already supplied by the worker. add_hint / add_task_hint can hand off references with traffic_refs; immediate explicit binding via report_finding traffic_refs / evidence_hint_id remains supported. Record TCP findings or findings without captured traffic normally; do not guess IDs or repeat probing just to capture traffic."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nWhen platform chat has no task context, do not call report_finding directly. Hand the finding off to the relevant existing task with add_task_hint, let that task's agent record it, and verify the result with list_task_findings."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nBefore deciding that the goal is complete, report or hand off all existing evidence. Do not end the task or cancel workers just because a finding was recorded in text while its evidence handoff is still incomplete. No captured traffic does not require waiting or forcing a capture."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**Automatic traffic association before reporting (enabled)**: You are responsible for verifying and binding traffic for the finding that triggered this run before writing the report. First obtain the explicit finding_id and finding_node_id from report_finding's returned JSON or get_task_node_detail / list_task_findings. Read the finding details, execution trace for its intent, and existing evidence list; prefer real IDs handed off by the reporter. If this verification used HTTP and traffic tools are available, filter candidates with traffic_search, then use traffic_get to verify each request/response actually supports the finding. Domain and time are filters only and do not establish association. Bind confirmed evidence in reproduction order with bind_finding_traffic(finding_id, traffic_refs), choosing baseline / proof / verification / supporting and explaining each role. Work only on this finding; do not create another finding or probe the target again. After a successful bind, call get_finding_traffic again for the latest version, read the required body, and pass the version actually read as evidence_version to update_finding_report (its finding_id parameter still expects finding_node_id). Do not append evidence that is already bound. For TCP, uncaptured traffic, unavailable tools, or no exact match, skip automatic binding, write the report based on text/command evidence, and explain why. Do not claim binding succeeded if it failed; preserve existing evidence and state in the report why no traffic was bound."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Optional: verified traffic references supporting a specific finding in this hint; preserve order. After handoff, report_finding can pass evidence_hint_id to include these references.", "items": obj(map[string]any{"traffic_id": str("Real traffic ID"), "role": str("baseline / proof / verification / supporting"), "note": str("What conclusion this traffic supports")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d must identify a hint in this task (inherited hints cannot be bound directly)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
