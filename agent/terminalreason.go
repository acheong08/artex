package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "The model completed this round normally but did not leave a text summary; use this round's tool-call records for facts and assets.",
	harness.ReasonMaxTurns:          "The turn limit (MaxTurns) was reached. The SDK ran the wrap-up and persisted facts and assets; the intent is marked exhausted so the planner can continue in another direction, not treated as a failure.",
	harness.ReasonTimeout:           "The run's wall-clock budget (MaxDuration) was reached. The active tool was interrupted and wrap-up began immediately to persist identified facts and assets; the intent is marked exhausted.",
	harness.ReasonModelError:        "The model or API call failed (network, authentication, rate limit, provider 5xx, etc.). After retries were exhausted, the intent was marked blocked. A transport failure means the intent was barely explored; inspect its execution with get_worker_trace before deciding whether to retry or change approach.",
	harness.ReasonBlockingLimit:     "The context reached its hard limit, so the request was blocked before being sent. Narrow the intent or reduce tool output.",
	harness.ReasonPromptTooLong:     "The prompt was too long and context-compaction retries were exhausted; execution cannot continue.",
	harness.ReasonImageError:        "The current model does not support this round's multimodal content. Switch to a vision-capable model or avoid tools that return images.",
	harness.ReasonStopHookPrevented: "A stop hook prevented this round from ending, and execution could not continue. Check whether the task Guard rules are too strict.",
	harness.ReasonHookStopped:       "A tool or hook stopped execution, for example because a target was out of scope or a command was disabled. Check the interception details in the last tool_result.",
	harness.ReasonAbortedStreaming:  "The run was cancelled while the model was streaming its response.",
	harness.ReasonAbortedTools:      "The run was cancelled during tool execution.",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "cancellation reason unavailable"
		}
		stage := "during execution"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "model response generation"
		case harness.ReasonAbortedTools:
			stage = "tool execution"
		}
		sum = "(Run interrupted: " + short + "; stopped during " + stage + progressSuffix(term, tr) + "; incomplete)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(Run budget limit reached (" + string(reason) + "); wrap-up persisted facts" + progressSuffix(term, tr) + "; no text summary was produced)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(No text summary; terminal state " + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + ")"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **Terminal state**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **Cancellation reason** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **Cancellation reason**: unavailable; the cancelling caller may not have attached a named cause with context.WithCancelCause\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **Underlying error**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **Partial output generated before cancellation**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **Turns completed**: %d model turns\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **Run duration**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **Token usage**: input %d / output %d / cache read %d / cache write %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **Tool calls**: the run ended before any tool call was issued\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **Tool running at interruption**: `%s` (ran for %s; **no result returned**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **Last tool before interruption**: `%s` (returned normally)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "The run context was cancelled, but the underlying process produced no Terminal event."
	}
	return "Unknown terminal state; the harness may have added a TerminalReason. Add it to reasonHint."
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; ran for " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
