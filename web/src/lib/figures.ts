import {
  fmtMoney,
  fmtMoneyAtLeast,
  fmtMoneyCut,
  fmtMoneyExact,
  fmtTokens,
  plural,
} from "@/lib/format";

import type {
  Group,
  HistoryTotalsResponse,
  RunningEntryResponse,
  RunResponse,
} from "@/api/types.gen";

/** The report fields a cost text reads; a `Summary` and a `SpendBucket` satisfy it too. */
export type CostFigures = Pick<
  Group,
  "cost_usd" | "tokens_unmeasured_runs" | "cost_unpriced_runs" | "cost_partial_runs"
>;

/** The report fields a token or cost text reads; a `Summary` satisfies it too. */
export type StatsFigures = CostFigures &
  Pick<Group, "tokens" | "succeeded" | "cost_per_succeeded_run_usd">;

/** The text of a token or cost figure that no run measured. */
export const NOT_MEASURED = "not measured";
/** The text of a cost the configured rates leave unpriced. */
export const UNPRICED = "unpriced";
const NOT_RECORDED = "not recorded";
const BASE_TIER = "base";

const LOWER_BOUND_NOTE =
  "At least this much. It leaves out runs Sortie could not price or measure.";

const LEAVES_OUT = "Leaves out spend Sortie could not price or measure";

/**
 * Words an estimated cost by its served status; a `partial` cost is a lower
 * bound and reads "at least". A status the client does not know reads "unknown".
 */
export function costText(status: string, usd: number | null): string {
  switch (status) {
    case "estimated":
      return usd === null ? "unknown" : fmtMoney(usd);
    case "partial":
      return usd === null ? "unknown" : fmtMoneyAtLeast(usd);
    case "unpriced":
      return UNPRICED;
    case "unmeasured":
      return NOT_MEASURED;
    default:
      return "unknown";
  }
}

/**
 * Gives the hover of a cost: its exact amount, and for a `partial` cost the
 * note that it leaves out spend. Gives undefined for a cost without an amount.
 */
export function costTitle(status: string, usd: number | null): string | undefined {
  if (usd === null) {
    return undefined;
  }
  switch (status) {
    case "estimated":
      return fmtMoneyExact(usd);
    case "partial":
      return `At least ${fmtMoneyExact(usd)}. ${LEAVES_OUT}.`;
    default:
      return undefined;
  }
}

/** Words the token figure of one run. */
export function runTokensText(run: RunResponse): string {
  if (!run.tokens_measured || run.uncached_tokens === null) {
    return NOT_MEASURED;
  }
  return fmtTokens(run.uncached_tokens);
}

/** Words the token figure of the session in flight. */
export function runningTokensText(running: RunningEntryResponse): string {
  const figure = running.uncached_tokens;
  if (!running.tokens_measured || figure === null) {
    return NOT_MEASURED;
  }
  return fmtTokens(figure);
}

/** Words the estimated cost of one run. */
export function runCostText(run: RunResponse): string {
  return costText(run.cost_status, run.estimated_cost_usd);
}

/** Words the token figure over every run of an issue. */
export function totalsTokensText(totals: HistoryTotalsResponse): string {
  if (totals.uncached_tokens === null) {
    return NOT_MEASURED;
  }
  return fmtTokens(totals.uncached_tokens);
}

/** Words the estimated cost over every run of an issue. */
export function totalsCostText(totals: HistoryTotalsResponse): string {
  return costText(totals.cost_status, totals.estimated_cost_usd);
}

/** Words the token figure of a report group or summary; a `base` tier records none. */
export function statsTokensText(f: StatsFigures, tier: string): string {
  if (tier === BASE_TIER) {
    return NOT_RECORDED;
  }
  if (f.tokens === null) {
    return f.tokens_unmeasured_runs > 0 ? NOT_MEASURED : NOT_RECORDED;
  }
  return fmtTokens(f.tokens.uncached);
}

function cacheReadsText(cacheReads: number | null, tokensFigure: number | null): string | null {
  // A zero also means an agent that reports no cache data, and a zero is a claim.
  if (tokensFigure === null || cacheReads === null || cacheReads === 0) {
    return null;
  }
  return fmtTokens(cacheReads);
}

/** Words the cache reads of one run, or `null` while there is nothing to state. */
export function runCacheReadsText(run: RunResponse): string | null {
  return cacheReadsText(run.tokens.cache_read_tokens, run.uncached_tokens);
}

/** Words the cache reads over every run of an issue, or `null` while there is nothing to state. */
export function totalsCacheReadsText(totals: HistoryTotalsResponse): string | null {
  return cacheReadsText(totals.tokens.cache_read_tokens, totals.uncached_tokens);
}

/** Words the cache reads of a report group or summary, or `null` while there is nothing to state. */
export function statsCacheReadsText(f: StatsFigures, tier: string): string | null {
  if (tier === BASE_TIER || f.tokens === null) {
    return null;
  }
  return cacheReadsText(f.tokens.cache_read, f.tokens.uncached);
}

/** Whether a report figure holds a cost that leaves out the spend of some runs. */
function lowerBound(f: CostFigures): boolean {
  return (
    f.cost_usd !== null &&
    (f.cost_unpriced_runs > 0 || f.cost_partial_runs > 0 || f.tokens_unmeasured_runs > 0)
  );
}

/** Words the estimated cost of a report group or summary; a cost that leaves runs out reads "at least". */
export function statsCostText(f: CostFigures, tier: string): string {
  if (tier === BASE_TIER) {
    return NOT_RECORDED;
  }
  if (f.cost_usd !== null) {
    return lowerBound(f) ? fmtMoneyAtLeast(f.cost_usd) : fmtMoney(f.cost_usd);
  }
  if (f.cost_unpriced_runs > 0) {
    return UNPRICED;
  }
  // A group of recorded CI results holds no measurable run, so it has no cost to state.
  return f.tokens_unmeasured_runs > 0 ? NOT_MEASURED : "none";
}

/**
 * Words the estimated cost of a report group or summary as the Analytics screen
 * shows it: a lower bound reads as its cut amount alone, because the screen
 * explains the bound on hover.
 */
export function statsCostFigure(f: CostFigures, tier: string): string {
  return tier !== BASE_TIER && f.cost_usd !== null && lowerBound(f)
    ? fmtMoneyCut(f.cost_usd)
    : statsCostText(f, tier);
}

/** Words the runs the token figure leaves out, or `null` while it leaves none out. */
export function statsTokensShortfall(f: CostFigures): string | null {
  return f.tokens_unmeasured_runs > 0
    ? plural(f.tokens_unmeasured_runs, "run not measured", "runs not measured")
    : null;
}

/** Words the runs the token and cost figures leave out, or `null` while they leave none out. */
export function statsShortfall(f: CostFigures): string | null {
  const parts = [
    statsTokensShortfall(f),
    f.cost_unpriced_runs > 0 ? plural(f.cost_unpriced_runs, "run unpriced", "runs unpriced") : null,
    f.cost_partial_runs > 0
      ? plural(f.cost_partial_runs, "run with a partial cost", "runs with a partial cost")
      : null,
  ].filter((part) => part !== null);
  return parts.length === 0 ? null : parts.join(", ");
}

/**
 * Gives the hover of a report cost: its exact amount, with the runs it leaves
 * out when it is a lower bound, or those runs alone while it has no amount.
 * Gives undefined on a `base` tier and when nothing is left out of a missing amount.
 */
export function statsCostTitle(f: CostFigures, tier: string): string | undefined {
  if (tier === BASE_TIER) {
    return undefined;
  }
  if (f.cost_usd === null) {
    return statsShortfall(f) ?? undefined;
  }
  const exact = fmtMoneyExact(f.cost_usd);
  const shortfall = statsShortfall(f);
  return shortfall === null ? exact : `At least ${exact}. ${LEAVES_OUT}: ${shortfall}.`;
}

/**
 * Words the cost per succeeded run of a report group or summary. A figure over a
 * lower-bound cost reads as its cut amount alone; gives `null` while no run succeeded.
 */
export function statsCostPerSucceededText(f: StatsFigures, tier: string): string | null {
  if (tier === BASE_TIER) {
    return NOT_RECORDED;
  }
  if (f.succeeded === 0) {
    return null;
  }
  const value = f.cost_per_succeeded_run_usd;
  if (value !== null) {
    return lowerBound(f) ? fmtMoneyCut(value) : fmtMoney(value);
  }
  return f.cost_usd !== null ? NOT_MEASURED : statsCostText(f, tier);
}

/** Explains in words why a report group or summary token figure is incomplete, or gives undefined while it is complete. */
export function statsTokensNote(f: CostFigures): string | undefined {
  return f.tokens_unmeasured_runs > 0
    ? "Leaves out runs whose tokens were not measured."
    : undefined;
}

/**
 * Explains in words what a report cost figure leaves out: a lower bound names
 * the spend it cannot count, a missing amount names why it is missing. Gives
 * undefined for a complete cost and on a `base` tier.
 */
export function statsCostNote(f: CostFigures, tier: string): string | undefined {
  if (tier === BASE_TIER) {
    return undefined;
  }
  if (f.cost_usd !== null) {
    return lowerBound(f) ? LOWER_BOUND_NOTE : undefined;
  }
  if (f.cost_unpriced_runs > 0) {
    return "The configured rates leave these runs unpriced.";
  }
  return f.tokens_unmeasured_runs > 0 ? "The agent reported no tokens for these runs." : undefined;
}

/** Explains in words that a cost per succeeded run is a lower bound, or gives undefined while it is not. */
export function statsCostPerSucceededNote(f: StatsFigures, tier: string): string | undefined {
  const isLowerBound = tier !== BASE_TIER && f.cost_per_succeeded_run_usd !== null && lowerBound(f);
  return isLowerBound ? LOWER_BOUND_NOTE : undefined;
}
