package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "Task paused by user",
		"The user paused the task through the task control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was cancelled; running intents return to the frontier (open) and will be claimed and rerun from the beginning when the task resumes.")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Task paused by orchestration agent",
		"The orchestration agent paused this task with the pause_task tool. This Planner/Worker run was cancelled; running intents return to the frontier (open) and will be rerun when the task resumes.")
	AbortTaskDeleted = cause("task_deleted", "Task deleted",
		"The task is being deleted (DELETE /api/tasks/{id}). The deletion barrier cancelled its running Planner, Workers, and main agent; this run's result will no longer be used.")
	AbortPausedOnReload = cause("paused_on_reload", "Task pause state restored by backend",
		"On startup, the backend restored the task's paused state from the database. This run was cancelled; normally no agents are running during recovery.")
	AbortGoalMet = cause("goal_met", "Planner determined that the task goal was met",
		"The planner determined that the task goal was met and marked the task done, then cancelled any remaining Workers. Their intents are marked stopped, not failed.")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task timeout wrap-up wait expired",
		"After the task timed out, the system waited for running Workers to wrap up gracefully, but the 90-second drain grace period was insufficient, so they were force-cancelled. Intents are marked exhausted; facts and assets written during wrap-up are preserved.")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Intent terminated by planner",
		"The planner terminated this intent with kill_work, usually because it went off course or is no longer useful. The intent is marked stopped and will not be claimed again automatically.")
	AbortWorkPausedByUser = cause("work_paused_by_user", "Worker intent paused by user",
		"The user paused the running Worker. This call was cancelled and the intent is now paused; the intent, facts, findings, and activity records already written are preserved. It will restart from the beginning when resumed.")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "Worker intent deleted by user",
		"The user deleted the running Worker. This call was cancelled; after the Worker exits its write section, the server applies the selected deletion mode: soft deletion only marks it deleted and preserves all output, while hard deletion cascades to remove the intent and downstream nodes supported only by it.")
	AbortWorkFinished = cause("work_finished", "Worker finished normally and released context",
		"The Worker finished normally, and the engine released its context resources in detachWork. This is not an interrupted run; if this appears in an interruption message, cancellation raced with cleanup.")
	AbortPausedRaceGuard = cause("paused_race_guard", "New run rejected while task is paused",
		"The engine rejected a new execution context while the task is paused, preventing a race between claiming work and pausing from starting another Worker. Already claimed intents return to the frontier.")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "Current conversation stopped by user",
		"The user clicked Stop to interrupt the main agent or session agent run. Activity records already produced are preserved; you can send another message.")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task paused and main-agent conversation cancelled",
		"Pausing the task also cancelled the running main-agent conversation. Activity records already produced are preserved; this message will not be replayed automatically when the task resumes.")
	AbortChatTurnFinished = cause("chat_turn_finished", "Conversation turn finished normally and released context",
		"This conversation turn finished normally, and the server is releasing its context resources. This is not an interrupted run; if this appears in an interruption message, cancellation raced with cleanup.")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running agents will be cancelled; after restart, remaining running intents are reset to open and rerun.")
	AbortRunHardTimeout = cause("run_hard_timeout", "Per-run hard-timeout backstop triggered",
		"The run exceeded its soft wall-clock budget plus the additional grace period. A model request or tool likely failed to return for an extended time, preventing normal wrap-up at a turn boundary. Check the last tool call that had not returned before interruption.")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context deadline reached",
			"Upstream context deadline reached, but the caller did not attach a named cause with WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Cancelling caller did not attach a named cause",
			"Upstream context was cancelled, but the caller did not attach a named cause with context.WithCancelCause; register a cause in agent/cancelcause.go and wire it to the cancellation point.", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
