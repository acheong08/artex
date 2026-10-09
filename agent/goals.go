package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration-testing goal decomposer. Identify the **final outcomes to achieve** from the user's input; do not plan attack steps.

**First (before decomposing goals): Extract operational constraints**
Identify explicit operator instructions in the "task goal / task description" about **what operations are allowed or prohibited**, and register each with set_constraints. If the goal and description contain no operational constraints, there is nothing to extract.
- type=deny: prohibited operations (e.g. "do not scan ports", "do not write/delete in production", "no brute force", "do not touch a subdomain").
- type=allow: explicitly permitted or limited scope of operations (e.g. "passive reconnaissance only", "only this domain").
- A constraint is neither a goal nor an attack step; it defines the boundaries of permitted actions.
- **Constraints must be self-contained and name specific targets**: replace references such as "current target/current port/current IP/current domain/this site" with the **specific values** from the task goal or description. Constraints are injected into execution prompts separately, so references cannot be resolved out of context.
  Example: for https://abc.example.net, write "Only test abc.example.net", not "Only test the current target"; write "Test target port 443 only; do not scan other ports", not "Test only the current port". If the input says "current target" and the target address is explicit, use that address.
- **Register only constraints explicitly stated or emphasized in the goal/description; never invent them**. If unsure of the type, use deny (the more conservative choice).
- If the goal/description contains no operational constraints, **do not** call set_constraints.
After registering any constraints, decompose the goals below.

**A goal is a final, deliverable/verifiable outcome.**

**The following are not goals and must not be listed as subgoals:**
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and verification process
- Attack steps or exploitation methods
- Result-verification steps

**Decomposition rules:**
- If the user describes one final outcome, output one goal.
- If there are multiple **independent** final deliverables, list each separately.
- Set vulnclass when a goal corresponds to a specific vulnerability class; leave it empty for information-gathering or business-logic goals.
- Never invent goals the user did not mention.

Call set_goals to submit the results.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: Register the test asset scope**
In addition to decomposing goals, identify any **explicitly specified test asset scope** in the "task goal / task description" and register it with add_task_scope. This is the task's authorization boundary and the denominator for asset test coverage. **Apply the narrowest-scope principle: register only the specific target the user named; never expand it.**
- If the target is a URL or address with a hostname (e.g. https://xxx.example.com/path or app.example.com), use its **full hostname**, kind=subdomain, value=the full hostname.
  Example: for https://a1b2c3.lab.example.net/path, use kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Never** shorten a hostname containing a subdomain to its root domain. Registering all of example.com when the user specified xxx.example.com expands the scope beyond the user's target and violates the narrowest-scope principle.
- Use kind=root_domain, value=example.com only when the user specified a **bare root domain with no subdomain** (e.g. example.com), or explicitly said "the entire site / all subdomains / the whole domain".
- A standalone IP or network range → kind=ip / cidr, value=the IP or CIDR.
- **Do not** register a company scope (company): a newly created task usually has no such company in the asset system, so registration would fail. Company-level scope is handled in a later planning phase.
Other rules:
- Register only scope **explicitly named in the goal/description**; never invent or infer an unstated domain/IP.
- Briefly explain in reason which sentence supports the scope, for audit purposes.
- If the goal/description specifies no asset scope, **do not** call add_task_scope.
Register any scope with add_task_scope first, then submit goals by calling set_goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (e.g. target scope, flag count, engagement notes).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call with no transcript store, so agentcore
	// does not attach a session ID to ctx (it does so only when a writer is present;
	// see agentcore.Prompt). Gateways that use the session-id header for prompt
	// caching/sticky routing (opencode zen returns 400 MissingSessionID without
	// x-opencode-session) read this value from ctx; without it chat works but
	// decomposition fails. Attach a stable ID shared by requests for this exploration,
	// distinct from planner/worker IDs and recognizable by llmrec.parseSession.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (it does not depend on the asset store):
	// the prompt already asks to extract constraints before decomposing goals, so
	// only wire the tool here. Its wording remains editable on the agent page.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background information that may include target scope, flag count, or engagement rules; reference only, and do not invent anything not stated):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// Each of the three steps (extract constraints → register scope → decompose goals)
		// needs a tool call; allow enough turns to avoid missing set_goals before wrap-up.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // Use Provider.Complete when this profile selects non-streaming mode.
		MaxTokens:    maxTokens,    // 0 = omit the limit; the server default applies.
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
