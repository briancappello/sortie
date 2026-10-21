import type { RunResponse } from "@/api/types.gen";

/** The status marks a row, an outcome, or a run line can carry. */
export type Mark =
  | "running"
  | "current"
  | "waiting"
  | "retry"
  | "held"
  | "needs"
  | "stopped"
  | "handed"
  | "succeeded"
  | "failed"
  | "degraded"
  | "clear"
  | "stalled";

/** The marks that state where work stands; `stalled` is a separate marker beside them, never a state. */
export type StateMark = Exclude<Mark, "stalled">;

const STATE_WORDS: Record<StateMark, string> = {
  running: "Running",
  current: "Current",
  waiting: "Waiting",
  retry: "Retry",
  held: "Held",
  needs: "Attention",
  stopped: "Stopped",
  handed: "Handed",
  succeeded: "Succeeded",
  failed: "Failed",
  degraded: "Degraded",
  clear: "Clear",
};

/** Gives the one word a status column shows for where work stands. */
export function stateWord(mark: StateMark): string {
  return STATE_WORDS[mark];
}

const RUN_STATUSES: Record<string, { word: string; mark: Mark | null }> = {
  succeeded: { word: "Succeeded", mark: "succeeded" },
  failed: { word: "Failed", mark: "failed" },
  ci_failed: { word: "CI failed", mark: "failed" },
  needs_person: { word: "Stopped for a person", mark: "stopped" },
  budget_stopped: { word: "Stopped at a budget", mark: "stopped" },
  cancelled: { word: "Cancelled", mark: null },
};

/** The persisted run statuses the dashboard words, in the order a menu lists them. */
export const RUN_STATUS_NAMES: readonly string[] = Object.keys(RUN_STATUSES);

/**
 * Gives the word and mark of a persisted run status. A run row serves its status
 * and no wording for it, so the client words the persisted statuses and shows any
 * other value as served with no mark.
 */
export function runStatus(status: string): { word: string; mark: Mark | null } {
  return Object.hasOwn(RUN_STATUSES, status) ? RUN_STATUSES[status] : { word: status, mark: null };
}

const DECLARED_OUTCOMES: Record<string, { word: string; mark: Mark }> = {
  blocked: { word: "Stopped to ask", mark: "stopped" },
  "needs-human-review": { word: "Ready for human review", mark: "handed" },
  "no-change-needed": { word: "No change needed", mark: "succeeded" },
};

/**
 * Gives the word and mark of a finished run: the outcome its agent declared when
 * the run is recorded as succeeded, else the word of its recorded status. Any
 * other status wins over a declaration, so a run whose handoff was withheld still
 * reads as failed.
 */
export function runOutcome(run: Pick<RunResponse, "status" | "declared_outcome">): {
  word: string;
  mark: Mark | null;
} {
  const declared = run.declared_outcome;
  if (
    run.status === "succeeded" &&
    declared !== null &&
    Object.hasOwn(DECLARED_OUTCOMES, declared)
  ) {
    return DECLARED_OUTCOMES[declared];
  }
  return runStatus(run.status);
}

/** Gives the statement the agent wrote with its declared outcome, or null when there is none to show. */
export function agentStatement(run: Pick<RunResponse, "agent_text">): string | null {
  const text = run.agent_text;
  return text === null || text.trim() === "" ? null : text;
}
