package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// P2 cross-task orchestration tools (see orchestration design §2 P2). These are
// host tools that need the Manager (any task's Store), Engine (pause), and task
// creation flow, so they live in the server layer. Read tools redirect existing
// per-task tools to the target task's store (create a temporary ToolSet and call
// the corresponding tool) to reuse identical logic; control tools (spawn/pause)
// call Manager/Engine directly. Like traffic tools, they are seeded into tools and
// bound per agent (visible only to agents with orchestration bindings).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // Platform operation tools (create/edit skills/tools/MCP) for Auto.
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] failed to load: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id is required"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task does not exist: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // Generic wake-up (writes without a dedicated callback use this; read tools are no-ops).
	tsx.SetNotifyHint(t.NotifyHint) // add_hint records "user added N strategic hints: ..." and wakes the planner.
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"List all tasks (id/description/goal/status/runtime/parent task/LLM profile). The orchestration agent uses this to get an overview, identify tasks that are stalled, and see which LLM each uses. Runtime in seconds: running = creation to now; terminal = creation to last activity. llm_profile is the profile used by the task's planner/worker; (active profile) means it follows the globally active profile.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(active profile)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d (deleted)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"List available LLM profiles: id, name, model, format, and whether each is currently active. Use an ID as spawn_task's llm_profile_id to assign a dedicated LLM to a child task (for example, a cheaper model for reconnaissance and a stronger model for exploitation). API keys are not included.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"Create a child task and start its exploration engine; returns task_id. Use this to delegate a piece of work (such as a challenge or goal) to an independent task. parent_ref is optional; set it to the current orchestration's parent task ID to link the tasks.",
		objSchema(map[string]any{
			"description":            strParam("Task description (short title)"),
			"goal":                   strParam("Task goal (what should be achieved)"),
			"parent_ref":             strParam("Optional parent task ID to link the tasks"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("Optional list of source task IDs to inherit read-only context from (up to %d). The child task can use assets and findings already discovered by these tasks as a starting point. Unlike parent_ref, which only links parent and child, this inherits content.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "Optional LLM profile ID for this child task's planner/worker (see list_llm_profiles); if omitted, inherits the parent task's profile, then falls back to the globally active profile"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "Optional task timeout in seconds. When reached, graceful wrap-up begins and the task enters the timeout terminal state; blank or 0 means no timeout"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "Optional planner heartbeat interval in seconds. If this much time has passed since the previous planning round ended or the task started, with no intervening trigger, another planning round begins (deadlock safeguard and supervision of in-flight workers). Blank or 0 defaults to 600 (10 min)."},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "Optional: enable for simple tasks to dispatch a seed intent (description + goal) at creation so the worker can begin testing without waiting for the first planner round. Defaults to false (standard plan-then-execute flow)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "Untitled task"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal is required"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// Read-only source task inheritance: enforce count limit and validate that
			// each ID is valid, unique, and exists, matching HTTP task creation.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("select no more than %d source tasks", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("source task ID is invalid or duplicated"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("source task #%d does not exist", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM profile #%d does not exist or has no API key configured", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// Reuse launchTask's shared post-creation flow with HTTP task creation
			// (server.go createTask): seed, visibly decompose goals in the background
			// (round 0/LLM steps/each goal), then engine.Run.
			// seed_first_intent defaults to false (plan before execution); simple tasks
			// can enable it to submit one work item immediately.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "Pause the specified task (stop its planner/worker loops).",
		objSchema(map[string]any{"task_id": strParam("ID of the task to pause")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task does not exist: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "Read the exploration graph overview for a task (same as graph_overview: asset counts, frontier, findings, coverage, etc.). Specify the task with task_id.",
		objSchema(map[string]any{"task_id": strParam("Task ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "Read confirmed findings for a task (including flags/PoCs; each entry includes id/task_id/intent_id/vulnclass/severity/summary/status). Specify the task with task_id.",
		objSchema(map[string]any{"task_id": strParam("Task ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "Add strategic hints to a task; its planner will read them when generating intents in the next round.\n"+
		"★ Prefer batching: submit multiple hints at once in the hints array (returns an ids array in matching order and length; failed entries have id=0). For a single hint, omit hints and provide the top-level text.",
		objSchema(map[string]any{
			"task_id":      strParam("Task ID"),
			"hints":        map[string]any{"type": "array", "description": "Preferred: an array of hints; each item has the same fields as the top level (text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("Hint text"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[Single hint] Hint text"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Optional asset IDs to anchor to (0, 1, or more; IDs from this task)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"Inspect the execution of a work item (intent) in a task: call get_task_worker_trace(task_id, intent_id) for step summaries, then pass step_ids=[...] to retrieve full content for those steps (up to 5 per call; only the first 5 are returned).",
		objSchema(map[string]any{
			"task_id":   strParam("Task ID"),
			"intent_id": map[string]any{"type": "integer", "description": "Intent ID (work item) in this task"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Optional step IDs to retrieve in full (up to 5 per call; only the first 5 are returned and the rest are listed in omitted_step_ids)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "List work items (intents) run in a task and their step counts to identify which ones are worth inspecting with get_task_worker_trace.",
		objSchema(map[string]any{"task_id": strParam("Task ID")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "Search all work-item execution traces in a task by keyword (returns matching step summaries and intent_id).",
		objSchema(map[string]any{"task_id": strParam("Task ID"), "q": strParam("Search keyword")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"Read the full content of an exploration graph node in a task (finding/fact/intent/goal: summary plus details/evidence/PoC). id is an exploration node ID (such as one returned by report_finding or listed by list_task_findings). Use this to retrieve complete finding evidence before writing a report.",
		objSchema(map[string]any{
			"task_id": strParam("Task ID"),
			"id":      map[string]any{"type": "integer", "description": "Exploration graph node ID (not an asset ID)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"Write or update the full Markdown report for a registered finding (replaces the entire previous report). Set finding_id to the ID returned by report_finding (the number in \"finding recorded: <id>\"). The report should include an overview, impact, reproduction steps, evidence/PoC, and remediation.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "Finding ID (the ID returned by report_finding)"},
			"report":           strParam("Full detailed report in Markdown format"),
			"evidence_version": map[string]any{"type": "integer", "description": "Evidence version returned by get_finding_traffic; prevents overwriting newer evidence changes"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // Reuse parsing for a number or numeric string.
			if nodeID <= 0 {
				return actool.Errorf("invalid finding_id"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("no finding record found for finding_id=%d (register it with report_finding first)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools are bound to the built-in Auto agent by default (it
	// is intended for platform operations). SeedTool affects only first insertion;
	// seedAutoDefaultBindings adds bindings to rows already seeded in older databases.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // Unbind list_facts/list_companies/list_worker_traces from worker defaults (one-time).
	s.seedWorkerReadbackRebind()  // Restore worker bindings removed by an old migration (one-time).
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // Add the operation-constraint extraction step to the goals prompt; append a new default for old databases (one-time).
	s.reseedMainAgentPrompt()         // Add a follow-up asking whether to create a goal after add_intent when existing goals are met (one-time).
	s.reseedPlannerPrompt()           // Rewrite valid reasons for zero intents and add quantitative acceptance checks (one-time).
	s.reseedWorkerPrompt()            // Add an evidence threshold for negative conclusions (one-time).
	s.seedReporterAgent()             // Seed the Report Writer agent, tool bindings, and finding trigger (one-time).
	s.upgradeReporterTriggerMessage() // Migrate old databases so the reporter returns evidence_version (one-time).
	s.seedFindingTrafficTools()       // Add optional evidence parameters and read-only evidence tools while preserving user configuration.
	s.seedFindingWorkflowTools()
	// Note: no migration is needed for pentest's default tool bindings. Fresh
	// initialization seeds list_assets/insert_assets/report_finding/list_findings/
	// list_companies for pentest, and there are no older databases to migrate.
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new parameter (e.g. spawn_task's llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// Also update selected built-in agent tools to their code defaults:
	//   - goal_met: older seeded descriptions misleadingly said "end this planning
	//     round", making the planner treat it as a way to end an empty round and
	//     mistakenly mark the whole task complete at startup.
	//   - insert_assets: add the related parameter (marks whether an asset belongs
	//     to the current task and whether it counts toward coverage). SeedTool only
	//     inserts once, so old schemas would otherwise miss the new parameter.
	//   - list_facts: add pagination and limit/before/q parameters. Old empty schemas
	//     would otherwise show "no parameters" in tool management and hide their
	//     descriptions from the model.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] refreshed orchestration/platform tool schemas to code defaults (one-time)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] failed to unbind goal_met from planner: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt updates the goals decomposer prompt to the current code
// default, which adds an operation-constraint extraction step
// (set_constraints) before decomposition. SeedPromptIfEmpty only inserts once, so
// existing databases would not receive it. Append a version and switch to it
// (ResetPromptToDefault); retain history so users can restore custom prompts.
// Guard with a settings flag and run once; bump the flag for future default changes.
// Fresh databases already seed the latest default and need no migration.
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Attempt only once, whether successful or not.
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // Fresh database has no agent row yet; seedPrompts will use the latest default.
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// A fresh database already has the latest default; do not append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to reset goals prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended new default version of goals prompt (adds a step to extract operational constraints; one-time)")
}

// reseedMainAgentPrompt updates the mainagent prompt to the current code default.
// The default now asks whether a direct add_intent should become an official goal
// after all goals are met. SeedPromptIfEmpty only inserts once, so existing
// databases would miss the change. Append a version and switch to it
// (ResetPromptToDefault), retaining history so users can recover custom prompts.
// A settings flag ensures this runs once. Fresh databases already seed the latest
// default. This mirrors reseedGoalsPrompt.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Attempt only once, whether successful or not.
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // Fresh database has no agent row yet; seedPrompts will use the latest default.
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// A fresh database already has the latest default; do not append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to reset mainagent prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended new default version of mainagent prompt (asks whether to add goals after completion; one-time)")
}

// reseedPlannerPrompt updates the planner prompt to the current code default.
// The default was streamlined: restraint now means deduplication only; it
// prioritizes depth over coverage, requires new work when goals remain unmet and
// no intents are running, and bounds negative-conclusion review. Bump the flag
// below (currently v2) when the default changes materially. SeedPromptIfEmpty
// only inserts once, so append a version and switch to it while retaining history
// for custom-prompt recovery. A settings flag runs this once; fresh databases
// already seed the latest default. This mirrors reseedGoalsPrompt.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Attempt only once, whether successful or not.
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // Fresh database has no agent row yet; seedPrompts will use the latest default.
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// A fresh database already has the latest default; do not append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to reset planner prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended new default version of planner prompt (streamlined, deduplication, depth-first, bounded negative-result review; one-time)")
}

// reseedWorkerPrompt updates the worker prompt to the current code default. The
// record_fact section removes the instruction to store negative conclusions as
// observations with tentative wording, and decouples confidence (observed/inferred)
// from whether the intent's methods were exhausted (which could mislead the planner).
// The facts list is narrowed to rare cases that are fully independent and cannot
// be combined. Bump the flag to v3 so existing databases receive the change.
// SeedPromptIfEmpty only inserts once, so append a version and switch while
// retaining history for recovery of custom prompts. Run once behind a settings
// flag; fresh databases need no migration. This mirrors reseedGoalsPrompt.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Attempt only once, whether successful or not.
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // Fresh database has no agent row yet; seedPrompts will use the latest default.
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// A fresh database already has the latest default; do not append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to reset worker prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended new default version of worker prompt (context lookup narrowed to list_assets/list_findings; removed list_facts/node_detail/asset_neighbors; one-time)")
}

// reporterToolCallMessage must always require one read of get_finding_traffic
// before writing a report. This tool is read-only and independent of capture
// settings, so it can read manually bound evidence whether automatic binding is
// enabled or not. If the prompt said to read only when automatic binding is on,
// the reporter would omit evidence_version by default; SetFindingReportVersionByNodeID
// would then store -1 using legacy semantics, leaving the finding and Markdown
// export permanently marked "Evidence changed; report needs updating" with no UI
// action to clear the status.
const reporterToolCallMessage = "A finding was just registered with report_finding. Read finding_id (the independent finding record ID) and finding_node_id (the exploration node ID) from the returned JSON. " +
	"First call get_finding_traffic(finding_id) to read the current evidence list and version (an empty list is normal; write the report anyway). " +
	"If automatic binding is enabled in the run settings, verify and bind traffic for this finding before reading the evidence. Use finding_node_id for node details. " +
	"Finally, save the report with update_finding_report(finding_id=finding_node_id, report, evidence_version=<version actually read>). " +
	"evidence_version is required; otherwise the report will remain marked as needing an update. Do not mix up the two IDs."

// Previous default trigger message (0.3.8 and earlier). Upgrade only records that
// still match it exactly; preserve user-edited messages.
const reporterToolCallMessageV1 = "A finding was just registered with report_finding. Read finding_id " +
	"(the number in the tool result \"finding recorded: <id>\") and the task ID from the trigger context, " +
	"write the detailed report for the finding, and save it with update_finding_report(finding_id, report)."

// upgradeReporterTriggerMessage updates the default reporter trigger message in
// existing databases. seedReporterAgent is guarded by reporter_agent_seed_v1 and
// creates the trigger only for a new agent, so upgraded databases would otherwise
// miss the new message. seedFindingTrafficTools adds evidence_version to the tool
// schema, but the reporter also needs to be told to use it. One-time; update only
// messages that have not been edited.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Attempt only once.
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] failed to read triggers: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // Preserve user edits and non-finding triggers.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] failed to upgrade trigger message: %v", err)
			return
		}
		log.Printf("[reporter] upgraded trigger message to read and return evidence_version")
	}
}

// seedReporterAgent creates a custom Report Writer agent (builtin=false, editable/
// deletable in the UI), binds update_finding_report and task-query tools, and adds
// a trigger on report_finding so every new finding starts a detailed report.
// One-time (settings-flag guarded); do not recreate it if the user deletes it.
// Depends on orchestration tools being seeded above so they can be bound.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Attempt only once, whether successful or not.

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // Key already exists (possibly created by the user); do not overwrite.
	}
	a, err := s.m.pg.CreateAgent("reporter", "Report Writer",
		"Detailed finding reports: triggered automatically when a finding is discovered; retrieves evidence and execution traces, writes a Markdown report, and saves it.")
	if err != nil {
		log.Printf("[reporter] failed to create agent: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] failed to seed prompt: %v", err)
	}
	// Trigger policy: parallel + none — one report per finding, with concurrent
	// independent runs for multiple findings. Merge must be none; otherwise the
	// default all mode combines a burst into one run and defeats parallelism.
	// maxParallel=5 limits concurrent report sessions and LLM calls.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] failed to set trigger run policy: %v", err)
	}
	// Bind tools needed to write reports and read evidence/execution context.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] failed to bind tools: %v", err)
	}
	// Trigger whenever report_finding is called (the tool returns "finding recorded:
	// <id>" with finding_id, and the task ID is also included in the trigger message).
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] failed to create trigger: %v", err)
	}
	log.Printf("[reporter] seeded the Report Writer agent and finding trigger")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] failed to apply default report_finding binding: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] failed to apply default report_finding binding: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] failed to apply default list_assets binding: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // Switch default binding from worker to planner.
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] failed to apply default add_company_scope binding: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] failed to unbind add_company_scope: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] failed to unbind %s from worker: %v", k, err)
			return // Leave the flag unset on error so the next startup retries.
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: add node_detail.
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] failed to rebind readback/detail tools: %v", err)
		return // Leave the flag unset on error so the next startup retries.
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: replace old asset tool names and add insert_assets/add_company_scope.
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// Asset tools: Auto often needs to inspect/register assets and manage company scope.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] failed to apply default bindings: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
