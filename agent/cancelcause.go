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
	AbortPausedByUser = cause("paused_by_user", "The user paused the task",
		"The user paused the task via the task control endpoint (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was actively cancelled; a running intent reverts to frontier(open) and, after the task resumes, is re-claimed and executed from the start")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "The orchestration agent paused the task",
		"The orchestration agent called the pause_task tool to pause this task. This Planner/Worker run was actively cancelled; a running intent reverts to frontier(open) and is re-executed after resume")
	AbortTaskDeleted = cause("task_deleted", "The task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier cancelled the Planner, Worker and main Agent running for this task; this run's result will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored paused task state",
		"On startup the backend restored the task's paused state from the state persisted in the database. This run was cancelled; normally there is no running Agent during the restore phase")
	AbortGoalMet = cause("goal_met", "The planner judged the task goal met",
		"The planner judged the task goal met and set the task to done, then cancelled the still-running Workers; these intents are marked stopped, not failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task timeout wrap-up wait exhausted",
		"After the task hit timeout it waited for running Workers to wrap up gracefully, but the 90-second drain grace was still insufficient, so a hard cancel was executed; intents are marked exhausted, and facts and assets already written during wrap-up are retained")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "The planner terminated this intent",
		"The planner called kill_work to actively terminate this intent, usually meaning the direction drifted or had no further value; the intent is marked stopped and is not re-claimed automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "The user paused this Worker intent",
		"The user paused the running Worker. This call was cancelled and the intent turned paused; the registered intent, facts, vulnerabilities and activity records are all retained, and after resume it is re-executed from the start")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "The user deleted this Worker intent",
		"The user deleted the running Worker. This call was cancelled; after the Worker exits the write region, the server handles the intent per the deletion mode the user chose — a soft delete only marks it deleted and keeps all output, while a hard delete cascades to remove the intent and the downstream nodes supported only by it")
	AbortWorkFinished = cause("work_finished", "Worker finished, context released",
		"The Worker finished normally and the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, it means cancellation raced with the teardown event")
	AbortPausedRaceGuard = cause("paused_race_guard", "Refused new run while task paused",
		"While the task is paused the engine refuses to issue a new execution context, to prevent a race between claim and pause from letting a Worker keep starting; a claimed intent reverts to frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "The user stopped this conversation turn",
		"The user clicked stop, actively aborting this main-Agent or session-Agent run. The activity records already produced are retained, and you can keep sending the next message")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task pause aborted the main-Agent chat",
		"When the user paused the task, the running main-Agent conversation was cancelled in sync. The activity records already produced are retained; after the task resumes, this turn's message is not replayed automatically")
	AbortChatTurnFinished = cause("chat_turn_finished", "Conversation turn finished normally",
		"This conversation turn finished normally and the server is releasing this turn's context resources. This is not a run interruption; if it appears in an interruption message, it means cancellation raced with the teardown event")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "The backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating or shutting down. All running Agents are cancelled; after restart, leftover running intents are reset to open and re-executed")
	AbortRunHardTimeout = cause("run_hard_timeout", "Single run hard-timeout backstop fired",
		"A single run exceeded the soft wall-clock budget plus extra grace, meaning a model request or some tool took too long to return, preventing the normal turn-boundary wrap-up from running. Focus on the last tool call that had not returned before the interruption")
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
		return "deadline_exceeded", "The upstream context hit its deadline",
			"The upstream context hit its deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "The canceller attached no named cause",
			"The upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; register the cause in agent/cancelcause.go and wire it to that cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
