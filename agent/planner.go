package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning to-do list (persists across wake-ups; written by you last round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed accordingly: dispatch an intent only for the next step whose prerequisites are complete / whose required facts exist. Use TodoWrite to update this list (mark steps satisfied by facts as completed). Do not dispatch steps already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 摘要).
//	"goal"    — the human (via 主 agent 的 set_goals) added one OR MORE goals in a
//	            single call (Goals = 本次新增的目标文本，1+ 条；set_goals 支持批量).
//	"goal_deleted" — the human deleted a goal from 总览的目标管理 (Detail = 被删目标文本).
//	"goal_edited"  — the human edited a goal from 总览的目标管理 (OldGoal→NewGoal 文本).
//	"cancelled" — the human deleted intent IntentID (Detail = 删除原因). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 专用：删除前捕获的意图摘要（真删除后节点已不存在，无法再查）
	Goals    []string // Kind=="goal" 专用：本次 set_goals 新增的目标文本（1 条或多条）
	OldGoal  string   // Kind=="goal_edited" 专用：修改前的目标文本
	NewGoal  string   // Kind=="goal_edited" 专用：修改后的目标文本
	Hints    []string // Kind=="hint" 专用：本次 add_hint 新增的提示文本（1 条或多条）
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Actual changes that triggered this round (read first, then decide whether to add directions)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a goal: %s — this is a new goal to achieve. Add exploration directions accordingly if none already cover it.", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s — all are new goals to achieve. Add exploration directions for each goal not already covered by an intent.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a strategic hint: %s — it has been added to the exploration graph. Adjust or add exploration directions accordingly if none already address it.", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s — all have been added to the exploration graph. Adjust or add exploration directions for each.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — it has been removed. Reassess the remaining goals/directions; do not dispatch intents for this goal.", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human changed a goal from “%s” to “%s” — adjust exploration directions for the new goal, and stop dispatching any directions that no longer apply.", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 意图内容优先用删除时捕获的 Summary（真删除后节点已不存在，intentSummary 查不到）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- The user deleted intent #%d: %s. Reason: %s. This intent has been removed and will not run; replan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished. Conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact IDs newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(For full details, use node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to retrieve output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(no output recorded for this work yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " … (truncated; use get_worker_output for the full output)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[Current situation (prefetched from graph_overview; equivalent to calling that tool. Use node_detail/list_facts, etc. as needed for details)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (段 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中间产物输出规约
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" in an authorized penetration testing system. You are woken frequently (whenever the graph changes). Your job is to review the situation → assess goals → add exploration intents **only when there are genuinely new, uncovered directions**. You are the planner, not the executor: this round's only outputs are to **create/clarify intents** or **assess goals**; never perform the work in the plan.

Task goal: {{.Goal}}

**How many intents to produce this round (decide this first):**
- **Hard requirement (highest priority):** If the **goal is not met** and there are **no open or running intents** (frontier_open=0 and running_intents is empty), you **must** produce at least one intent that advances the goal. With no running work to wait for and no queued directions, zero intents would leave the task stalled. Even if all known directions appear only in recent_done, use the done/exhausted/blocked guidance below to open another intent or continue one.
- Outside that hard requirement, **zero intents is a normal outcome, but requires a valid reason** (do not default to "fewer is safer"): (1) **Already covered** — all directions you considered are being handled by open/running intents (creating an existing intent again with different wording is a serious error); or (2) **Waiting on dependencies** — the next step depends on output from running work that has not arrived yet (dispatching it now would leave downstream work spinning without prerequisites; wait until the graph changes and you wake again).
- Conversely, dispatch a direction when there is a genuinely **uncovered direction that does not depend on running work**, or the goal is not met and in-scope areas remain untested. Do not treat zero intents as the lazy default.

**Decision process for each wake-up:**

1. **The complete situation is included below this prompt** (the graph_overview response; do not call it again): task (original title + goal/root node), asset counts, goals and states, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, which may suggest a direction to explore), facts (exploration facts, distinct from findings), and recent_facts ({id,summary,confidence?}).
   - **Scope:** Exploration nodes (goals/intents/facts/findings) belong only to this task. **The asset graph is globally shared** across tasks (asset counts are global in-scope counts, not unique to this task); ignore assets unrelated to this task.
   - **Lineage:** Each intent has parents (upstream facts/intents it derives from) and yields (downstream facts/findings it produced); each recent_facts entry has from_intent. Use these to understand which directions produced which facts and whether they can be combined into new directions.
   - **Negative/uncertain observations** (e.g. "port closed"/"not injectable" in recent_facts) are worker observations, not definitive conclusions. Before relying on one, inspect its evidence with node_detail(id). Treat a direction as temporarily blocked only when evidence is strong, confidence=observed, and reasonable methods were exhausted. If evidence is missing, the claim only "looks like" something, it was probed just once, or confidence=inferred, treat it as **unexplored**. If in scope and not covered by another intent, normally dispatch one verification intent to confirm or refute it (**verify a given negative direction at most once**; if it remains negative with reasonable evidence, accept the conclusion and do not dispatch again).
   - **Fetch deeper details only when needed:** list_facts (paginated, newest first, 20 by default, q filtering, before cursor, total/has_more), list_findings (all findings), node_detail(id) (full evidence/details; lists/recent_facts include summaries only), list_assets (pull: search with q, filter by type/company_id/task_id, paginate, or fetch by id/ids), and asset_neighbors. The asset graph is shared; do not fetch everything by default.

2. **Assess goals (core responsibility):** The goals field already contains goals and their states. For an unmet goal proven by a finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When you mark the last unmet goal met, the system automatically marks the entire task complete** — task completion is driven by proving goals individually; there is no other "complete everything" mechanism.
   - ⚠️ **Check quantitative acceptance criteria (never mark met prematurely):** If a goal contains a measurable condition (X% coverage, N flags, a particular privilege), **check** the measured values in the graph_overview above before calling prove_goal (coverage.pct, findings_total, etc.). If the criterion is unmet, **do not** call prove_goal; dispatch intents to close the gap. Do not mark met early because it is "mostly achieved" or "the core objective is done." Example: a 100% coverage goal with coverage.pct=40% is unmet; continue testing.

3. **(Optional, very lightweight, startup only) Probe to understand the target:** Only when the graph has almost no facts (recent_facts is basically empty and the task has just started) and the situation alone is insufficient to make the initial intent specific, perform a very small number of read-only probes with Bash (e.g. 1–2 curl requests to inspect the homepage/fingerprint). **The only permitted output is a more precise intent description** — not finding, verifying, or exploiting a vulnerability, and not enumerating endpoints/directories/parameters (those are worker tasks; dispatch them as intents). Hard boundaries:
   - If the graph already has a worker-produced fact (facts>0 / recent_facts is non-empty), **do not probe the target yourself**. Base decisions on existing facts; this round's only outputs may be "dispatch new intents" or "finish." To investigate a lead further, dispatch an intent for a worker instead of curling it yourself.
   - Even at startup, stop after at most 3 probes; their sole purpose is to clarify the initial intent. If you find yourself doing "in-depth verification" rather than "quickly choosing a direction" (enumerating endpoints/directories, trying IDs one by one, decoding a chain, repeatedly probing the same endpoint, or testing injection/authorization bypass/vulnerabilities — all substantial worker tasks), stop immediately and write it up as an intent.
   - Do not probe when the existing facts/situation are sufficient to make the decision.

4. **Choose which new directions to add:** **Restraint means only "do not duplicate existing intents," not "dispatch as few as possible."** When goals are unmet, the default question is "What deeper, more effective, still-uncovered approaches could move us closer to the goal?" — not "Can we wrap up?" Intents are **open-ended exploration directions**, not a fixed menu. Choose directions based on known facts, assets, and goals, and compare each with open + running + recent_done:
   - Covered by an existing open/running intent → do not create another; it is already being handled.
   - Present in recent_done → **first inspect that intent's state (included for each entry) to see how it ended, then decide**:
     · **done (completed normally):** Already covered; do not dispatch it unchanged. Judge whether it was a dead end from the facts it yielded, not from its state. Dispatch it again only if a **materially new mechanism** appears (new facts/assets/parameters or a clearly different approach), and explain the difference from the prior attempt in summary. Rewording it or saying "maybe it will work this time" is not enough; do not retry.
     · **exhausted (budget ran out mid-probe and only partial results were written) / blocked (model or network failure prevented meaningful exploration):** Both ended prematurely and have incomplete information. Use get_worker_trace / get_worker_output to see what was actually done and where it got stuck, then decide: near a breakthrough but cut off by budget → dispatch "continue from the previous progress"; purely external failure → dispatch the same direction again; repeatedly stuck at the same point → change the approach/direction. Base this on actual trace progress, never state alone.
   - A completely new direction not covered by any intent → create it.
   - If all known directions are covered by open/running intents, produce none and finish (there is work running/queued, so wait for it to progress). But if only recent_done intents cover them and there are no open/running intents while the goal is unmet, follow the hard requirement above and open a new intent or continue one.
   - **Depth over coverage:** coverage is a floor/acceptance criterion, not an exploration goal itself. After finding a high-value entry point (potentially leading to RCE/privilege escalation/data exposure), dispatch an intent to **drive that path deeper**, rather than spreading out to equalize coverage and shallowly test each asset.
   - **Keep paths diverse; do not converge too early:** When goals are unmet and existing intents cluster around one path/entry point, prioritize an uncovered direction that is **fundamentally different** (another entry surface/asset class/exploit chain) rather than another synonymous intent on the same path (judge substantive differences, not wording). Do not create it if already covered. Ideally maintain 2–3 paths with different mechanisms (e.g. upload-chain and authentication-bypass approaches) in parallel, then focus resources after one provides evidence of **progress toward the goal**. **Diversity always yields to the operation constraints above**: never create an intent involving a surface/port/host/action excluded by a constraint, even if it is fundamentally different.

   **Serial exploit chains: dispatch step by step, not in parallel.** For a strongly dependent chain (①→②→③, where each step needs the prior step's actual output), do not dispatch all steps at once; downstream steps would lack prerequisites and just repeat work or spin. Use TodoWrite to track the full chain (one item per step), dispatch only the next step whose prerequisites are satisfied this round (usually the first), then dispatch the following step after a fact is produced and the next wake-up includes the to-do list; mark satisfied steps completed. Do not split one action into two steps (e.g. "identify the trigger" and "trigger it" are one step). Use parallel intents only for independent dimensions (e.g. enumerate unrelated endpoints).

5. **Submit:** Make **one** batched add_intent call for the new directions selected (intents array, up to the 4 highest-value intents; do not call once per intent):
   - **summary**: Describe the direction in one natural-language sentence (full target address + what to do + why); do not force it into a fixed category. Deduplication relies primarily on comparing this with existing intents.
   - **asset_ids**: IDs of the target assets to test/attack for this direction (include whenever possible; 0/1/multiple, from list_assets). Always provide IDs when the direction concerns specific assets (sites/endpoints/parameters/hosts) so coverage can be deduplicated and the intent linked into asset lineage; include all relevant IDs when it spans multiple assets. Leave empty only for global reconnaissance with no specific target assets.
   - **parent_ids**: Optional upstream node IDs from which this direction was derived (0/1/multiple). Include all facts that jointly produced an intent; also include the ID of an upstream intent/finding when the direction derives from it. Leave empty for a new top-level direction.

Do not duplicate or force intents; but when goals are unmet and there is an uncovered, deeper approach, dispatch it. Be concise, focused, and efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 领域工具 + 基础默认工具集（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 资产覆盖度功能关闭时剔除 add_task_scope/list_untested_assets（不入 prompt）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 关键态势（刚完成的意图 + 预取的完整图）改放【本轮 user 输入】(见下方 input)，system
	// 只留静态规划正文。move-out 让 system 每轮稳定、更利于缓存；代价是若单轮变长，态势可能
	// 被 compaction 压缩（planner 单轮通常短，风险低）。situational 会拼进下方 input。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 任务级 deadline / 终局模式(经 ctx 注入,见 taskclock.go)。终局那一轮把任务超时
	// planner 收尾词作为【本轮操作指令】拼进本轮 user 输入(随 situational),让它只做最后
	// 目标判定、不产新意图。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【任务终局收尾（本轮特殊指令，覆盖上面的常规规划流程）】：" + resolveTaskTimeoutWrapup("planner")
	}
	// 本任务的工作目录 <workDir>/tasks/<taskID>，先建好。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作约束(若有)注入系统提示,框定探索边界
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 无自身墙钟预算;有 deadline 时把 MaxDuration 夹逼到剩余,让在跑的规划轮在
	// 任务到点时进收尾(因超时→任务超时词,因步数→per-run 词)。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 走记录代理留痕；载入代理 CA 验证 MITM 重签的 HTTPS 证书
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 联网搜索(可选)。ddgs 无需 key；brave-free 需 BraveKey；tavily 需 TavilyKey。
		// WebSearchProxy 是独立出口代理(http/https/socks5)，与记录流量的 MITM 代理无关；空则直连。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 子命令默认走代理+信任 CA
		WorkingDir:            taskDir,                              // 本任务工作目录 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=不限;有 deadline 时=距 deadline 剩余
		Compaction:            compactionConfig(p.compactionWindow()),
		// 跨唤醒共享的规划待办：让串行链在多轮之间保留（session 是新的，store 不是）。
		Todos: p.todoFor(ts.ID()),
		// 命中【本轮】步数预算→ SDK 跑收尾:把本轮已想清楚的结论落地(该派的 add_intent、
		// 能证的 prove_goal、串行链记 TodoWrite),而非停止规划——planner 之后仍会被反复唤醒。
		// clamped(被任务 deadline 夹逼)时改用 PromptByReason(见 wrapupSettlementForTask)。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = 不发上限,由服务端默认值决定
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 实验功能:开启后由 noa 接管上下文压缩(归档集中在 <workDir>/noa/<SessionID> 下,持久)。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 态势（刚完成的意图 + 完整图）现在拼进本轮 user 输入（见下方 input）。user 里还有
	// 指令 + 跨唤醒待办（todo 是模型自己的规划便签，可再生，放 user 即可）。
	// 开场白按「本轮有无具体变动」分两种：有变动 → 指向下方【实际变动】块；无变动
	// (心跳定时巡检 / hint / 恢复等) → 别谎称"图发生了变化",转而提示顺带复查在跑意图。
	lead := "A concrete change just occurred (see [Actual changes that triggered this round] below). Plan the next steps accordingly:"
	if len(triggers) == 0 {
		lead = "This wake-up was triggered by a **scheduled check (heartbeat) / no concrete change signal** — the graph may not have changed. Also review running intents: use steer_work to correct those stalled or off course, and kill_work to stop those going in a completely wrong direction; then reassess the goals and decide whether to add directions:"
		// 心跳/无变动唤醒时,若全图已无任何 open 或 running 意图 → 探索已停摆(没 worker 在跑、
		// 也没排队方向)。明确告知 planner 并强制其本轮补出新方向,别只复查在跑意图后空转一轮。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This wake-up was triggered by a **scheduled check (heartbeat)**, and there are currently **no open or running intents** — no worker is running and no directions are queued, so exploration has stalled. You **must** produce one or more new intents this round that advance the goals and **do not duplicate** existing graph intents (you may not produce zero intents). First determine from the situation below whether the goals are met; if not, add directions immediately:"
		}
	}
	input := lead + situational + "\n\nBased on the situation above, assess the goals. When a goal is **truly achieved** (the desired result was obtained / the target vulnerability was confirmed), mark it met with prove_goal. **Hard requirement: if the goals are not met and there are no open or running intents (frontier_open=0 and running_intents is empty), you must produce at least one intent that advances the goals this round — there is no running work to wait for and no queued direction, so zero intents would leave the task stalled. You may produce no new intents only when existing open/running intents are making progress or the goals are met.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration 现在会在墙钟到点打断在跑工具并就地进收尾(在活 ctx 上),单轮卡死不再
	// 绕过收尾,无需外部硬 ctx 兜底。ctx 只承载 pause / kill / shutdown。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
