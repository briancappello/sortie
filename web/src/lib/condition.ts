import type { StateResponse } from "@/api/types.gen";

/** The one state the status card, narrow bar, title, and favicon follow. */
export type Condition = "degraded" | "needs" | "clear";

/** Derives the condition: a failing check outranks attention items, which outrank all clear. */
export function condition(state: StateResponse): Condition {
  const { health } = state;
  if (health !== undefined) {
    const checks = [health.database, health.preflight, health.workflow, health.poll, health.scm];
    if (checks.some((check) => check !== null && check.status === "fail")) {
      return "degraded";
    }
  }
  return state.counts.attention > 0 ? "needs" : "clear";
}

/** Words the condition for the status card and the title. */
export function conditionWord(c: Condition): string {
  switch (c) {
    case "degraded":
      return "Degraded";
    case "needs":
      return "Needs attention";
    case "clear":
      return "All clear";
  }
}
