package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up (settlement) prompts are injected during the SDK settlement phase when an
// agent is terminated for exhausting its turn budget (MaxTurns) or timing out
// (run_seconds/MaxDuration). They direct the agent to persist identified results
// before producing a summary, avoiding incomplete runs.
//
// Each agent's wrap-up prompt can be overridden in the admin UI (stored in
// agents.wrapup_prompt); empty values use the built-in default. Only the prompt body
// is editable. Disabled tools and the wrap-up turn budget are fixed in code.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts indexed by agent key. worker reuses the historical
// hard-coded settleWrapUpPrompt (defined in worker.go); planner and mainagent have
// their own. Unmatched keys (custom agents) use the generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults are the built-in turn budgets for each agent's wrap-up phase
// (overridable by a positive value in the admin UI). Each gets 10 turns to persist
// results; unmatched keys use genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "You are about to run out of planning turns — note that only **this round** is ending. The system will wake you again as the situation changes; the task itself is not ending, and you do not need to wrap up the overall plan now. Persist the conclusions you have reached this round so it is not wasted, but **do not force intents just to wrap up** (zero intents this round is perfectly normal): (1) If you have identified an exploration direction that should be dispatched now, submit it with one batched add_intent call; do not hold back a direction you have already decided on. (2) For goals proven met by a finding/fact, call prove_goal to mark them met; do not miss any. (3) If you identified a serial exploit chain that needs to be broken into steps, record it with TodoWrite so you can dispatch the next step after the next wake-up. Then end this round without a summary."

const mainAgentWrapUpDefault = "You are about to run out of turns, and this interaction is ending. Do not start any new exploration or actions. **In one standalone plain-text sentence**, summarize the current progress, key conclusions, and recommended next step for the user."

const genericWrapUpDefault = "You are about to be terminated because your budget is exhausted. First write back any completed results that have not been persisted, then **finish with one standalone plain-text sentence** summarizing what you did and the key conclusions you reached (this sentence will be shown as the result of this run)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up prompts (see the task-timeout design doc) ----------
//
// These differ from per-run prompts: per-run means "this run exhausted its budget";
// task timeout means "the entire task is ending". Their instructions can be opposite
// (especially for planner: per-run says continue planning, while task timeout says
// stop planning and make a final assessment). Configured only for worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride are DB overrides for task-timeout
// prompts and turn budgets (wired to agents.task_timeout_wrapup_prompt / _max_turns,
// worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (this is not your run budget; the overall exploration time is up). This is your final opportunity: (1) Persist **everything** you identified but have not yet written back — new assets with insert_assets, exploration conclusions/facts with record_fact, and confirmed vulnerabilities with report_finding; (2) Do not start any new commands or probes; (3) **Finish with one standalone plain-text sentence** summarizing the key conclusions for this intent."

const plannerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (this is not just this round; the whole task is terminating). Based on **all** current facts and findings, make a final goal assessment: call prove_goal to mark each goal met when evidence proves it has been achieved; do not miss any. **Do not generate any new intents** (they will not be executed at this point). End after the assessment; no summary is needed."

// TaskTimeoutWrapupDefault returns an agent's built-in task-timeout wrap-up prompt
// (for admin UI placeholders and restoring defaults).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // Unconfigured agents (mainagent/chat) return an empty string.
}

// resolveTaskTimeoutWrapup resolves a non-empty DB override before the built-in
// default. Empty means the agent has no task-timeout prompt (not worker/planner);
// callers should fall back to the per-run prompt.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // Default to the per-run turn budget.
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true: this run was limited by the task deadline. Timeout means the
//     task ended and uses the task-timeout prompt; MaxTurns means the run's turns
//     expired first while the task still had time, so use the per-run prompt.
//   - clamped=false: the task deadline is not close; both reasons use the per-run
//     prompt (equivalent to wrapupSettlement).
//
// PromptByReason lets the harness select at wrap-up time using the actual reason,
// avoiding a mismatch based on the reason predicted at build time.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // Fallback (also used for either reason when not clamped).
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // Task deadline reached.
				harness.ReasonMaxTurns: perRun, // Turn budget reached first; task still has time.
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
