import { fmtDue, fmtDuration, parseInstant } from "@/lib/format";

import type {
  AttentionItemResponse,
  BudgetExhaustedResponse,
  ConfigResponse,
  PendingReactionResponse,
  ParkedEntryResponse,
  RetryEntryResponse,
  RunningEntryResponse,
  StageHopResponse,
  StagePositionResponse,
  StateResponse,
} from "@/api/types.gen";
import type { Mark } from "@/lib/run-status";

/** The row list of the state that decided a row. */
export type RowKind =
  | "running"
  | "retrying"
  | "budget_exhausted"
  | "parked"
  | "pending_reactions"
  | "between_stages";

/** The lifecycle phase a row belongs to; the Active chips filter on it. */
export type Phase = "running" | "pull_request" | "between_stages" | "retry" | "needs_attention";

/** Lists every phase in the order the chips appear. */
export const PHASES: readonly Phase[] = [
  "running",
  "pull_request",
  "between_stages",
  "retry",
  "needs_attention",
];

/** One issue in Active, merged from the state's row lists. */
export interface ActiveRow {
  issueId: string;
  identifier: string;
  displayKey: string;
  title: string | null;
  kind: RowKind;
  phase: Phase;
  mark: Mark;
  statusText: string;
  statusTextFull: string;
  route: string | null;
  agentKind: string | null;
  rule: string | null;
  timeBase: string | null;
  dispatchedAt: string | null;
  acknowledgedAt: string | null;
}

/** The position an order memory holds for a row. */
export interface OrderKey {
  dispatchedAt: number | null;
  identifier: string;
  issueId: string;
}

/** One entry of the Rule menu; `group` is the first rule of the chain a header or member belongs to. */
export interface RuleOption {
  kind: "any" | "chain" | "member" | "caption" | "rule" | "none";
  value: string;
  label: string;
  count: number;
  group: string | null;
}

type Deciding =
  | { kind: "running"; entry: RunningEntryResponse }
  | { kind: "retrying"; entry: RetryEntryResponse }
  | { kind: "budget_exhausted"; entry: BudgetExhaustedResponse }
  | { kind: "parked"; entry: ParkedEntryResponse }
  | { kind: "pending_reactions"; entry: PendingReactionResponse }
  | { kind: "between_stages"; entry: StageHopResponse };

interface RuleSource {
  stage_position: StagePositionResponse | null;
  rule_name?: string;
}

const KINDS: readonly RowKind[] = [
  "running",
  "retrying",
  "budget_exhausted",
  "parked",
  "pending_reactions",
  "between_stages",
];

const DEFAULT_RULE = "default";

function isNamedRule(name: string | undefined): name is string {
  return name !== undefined && name !== "" && name !== DEFAULT_RULE;
}

function entriesOf(state: StateResponse, kind: RowKind): Deciding[] {
  switch (kind) {
    case "running":
      return (state.running ?? []).map((entry) => ({ kind, entry }));
    case "retrying":
      return (state.retrying ?? []).map((entry) => ({ kind, entry }));
    case "budget_exhausted":
      return (state.budget_exhausted ?? []).map((entry) => ({ kind, entry }));
    case "parked":
      return (state.parked ?? []).map((entry) => ({ kind, entry }));
    case "pending_reactions":
      return (state.pending_reactions ?? []).map((entry) => ({ kind, entry }));
    case "between_stages":
      return (state.between_stages ?? []).map((entry) => ({ kind, entry }));
  }
}

/** Gives the phase of a row; a reaction row is needs-attention only once it escalated. */
export function phaseOf(kind: RowKind, escalatedAt: string | null): Phase {
  switch (kind) {
    case "parked":
    case "budget_exhausted":
      return "needs_attention";
    case "pending_reactions":
      return escalatedAt === null ? "pull_request" : "needs_attention";
    case "running":
      return "running";
    case "retrying":
      return "retry";
    case "between_stages":
      return "between_stages";
  }
}

/** Gives the mark of a row and, while an item of the issue was acknowledged, when. */
export function rowMark(
  kind: RowKind,
  issueId: string,
  attention: readonly AttentionItemResponse[],
): { mark: Mark; acknowledgedAt: string | null } {
  const items = attention.filter((item) => item.issue_id === issueId);
  if (items.some((item) => item.acknowledged_at === null)) {
    return { mark: "needs", acknowledgedAt: null };
  }
  const acknowledged = items.find((item) => item.acknowledged_at !== null);
  if (acknowledged !== undefined) {
    return { mark: "held", acknowledgedAt: acknowledged.acknowledged_at };
  }
  if (kind === "running") {
    return { mark: "running", acknowledgedAt: null };
  }
  return { mark: kind === "retrying" ? "retry" : "waiting", acknowledgedAt: null };
}

/** Words a row's place in a chain or its rule, or `null` when neither is worth showing. */
export function routeText(entry: RuleSource, config: ConfigResponse | undefined): string | null {
  const position = entry.stage_position;
  if (position !== null) {
    const index = (position.chain ?? []).indexOf(position.current);
    const length = (position.chain ?? []).length;
    if (index >= 0 && length > 1) {
      return `${position.current}, ${index + 1} of ${length}`;
    }
    return position.current;
  }
  const hasRules = (config?.dispatch.rules ?? []).length > 0;
  return hasRules && isNamedRule(entry.rule_name) ? entry.rule_name : null;
}

/** Gives the value the Rule menu filters a row on. */
export function ruleOf(entry: RuleSource): string | null {
  if (entry.stage_position !== null) {
    return entry.stage_position.current;
  }
  return isNamedRule(entry.rule_name) ? entry.rule_name : null;
}

function timeBaseOf(deciding: Deciding): string | null {
  switch (deciding.kind) {
    case "running":
      return deciding.entry.started_at;
    case "retrying":
      return deciding.entry.due_at;
    case "budget_exhausted":
      return deciding.entry.exhausted_at;
    case "parked":
      return deciding.entry.parked_at;
    case "pending_reactions":
      return deciding.entry.created_at;
    case "between_stages":
      return deciding.entry.hopped_at;
  }
}

function decidingFor(candidates: Deciding[]): Deciding {
  const first = candidates[0];
  if (first.kind !== "pending_reactions") {
    return first;
  }
  return (
    candidates.find((c) => c.kind === "pending_reactions" && c.entry.escalated_at !== null) ?? first
  );
}

function toRow(deciding: Deciding, state: StateResponse): ActiveRow {
  const { entry, kind } = deciding;
  const escalatedAt = deciding.kind === "pending_reactions" ? deciding.entry.escalated_at : null;
  const { mark, acknowledgedAt } = rowMark(kind, entry.issue_id, state.attention ?? []);
  return {
    issueId: entry.issue_id,
    identifier: entry.issue_identifier,
    displayKey: entry.display_identifier || entry.issue_identifier,
    title: entry.issue_title || null,
    kind,
    phase: phaseOf(kind, escalatedAt),
    mark,
    statusText: entry.status_text,
    statusTextFull: entry.status_text_full,
    route: routeText(entry, state.config),
    agentKind: entry.agent_kind || null,
    rule: ruleOf(entry),
    timeBase: timeBaseOf(deciding),
    dispatchedAt: entry.dispatched_at,
    acknowledgedAt,
  };
}

/** Merges the state's row lists into one row per issue; the earliest list decides which entry speaks for it. */
export function mergeRows(state: StateResponse): ActiveRow[] {
  const candidates = new Map<string, Deciding[]>();
  for (const kind of KINDS) {
    for (const deciding of entriesOf(state, kind)) {
      const list = candidates.get(deciding.entry.issue_id) ?? [];
      list.push(deciding);
      candidates.set(deciding.entry.issue_id, list);
    }
  }
  return [...candidates.values()].map((list) => toRow(decidingFor(list), state));
}

/** Words the time cell of a row on the server clock. */
export function timeCell(row: ActiveRow, now: number): string {
  const base = parseInstant(row.timeBase);
  if (base === null) {
    return "unknown";
  }
  return row.kind === "retrying" ? fmtDue((base - now) / 1000) : fmtDuration((now - base) / 1000);
}

function compareKeys(a: OrderKey, b: OrderKey): number {
  if (a.dispatchedAt !== b.dispatchedAt) {
    if (a.dispatchedAt === null) {
      return -1;
    }
    if (b.dispatchedAt === null) {
      return 1;
    }
    return a.dispatchedAt - b.dispatchedAt;
  }
  if (a.identifier !== b.identifier) {
    return a.identifier < b.identifier ? -1 : 1;
  }
  if (a.issueId !== b.issueId) {
    return a.issueId < b.issueId ? -1 : 1;
  }
  return 0;
}

/**
 * Orders rows by the key remembered when each first appeared, so a row never
 * moves while it stays active. It writes to `memory`; call it on live data
 * only, never on a held snapshot.
 */
export function orderRows(memory: Map<string, OrderKey>, rows: ActiveRow[]): ActiveRow[] {
  const present = new Set(rows.map((row) => row.issueId));
  for (const issueId of memory.keys()) {
    if (!present.has(issueId)) {
      memory.delete(issueId);
    }
  }
  const keyed = rows.map((row) => {
    let key = memory.get(row.issueId);
    if (key === undefined) {
      key = {
        dispatchedAt: parseInstant(row.dispatchedAt),
        identifier: row.identifier,
        issueId: row.issueId,
      };
      memory.set(row.issueId, key);
    }
    return { row, key };
  });
  return keyed.sort((a, b) => compareKeys(a.key, b.key)).map(({ row }) => row);
}

function countRows(rows: readonly ActiveRow[], matches: (rule: string | null) => boolean): number {
  return rows.filter((row) => matches(row.rule)).length;
}

function chainsOf(config: ConfigResponse): string[][] {
  return (config.dispatch.chains ?? []).filter((chain) => chain.length > 0);
}

function matchesQuery(label: string, query: string): boolean {
  return label.toLowerCase().includes(query.toLowerCase());
}

function filterOptions(options: RuleOption[], query: string): RuleOption[] {
  if (query === "") {
    return options;
  }
  const selectable = options.filter((o) => o.kind !== "any" && o.kind !== "caption");
  const kept = options.filter((option) => {
    switch (option.kind) {
      case "any":
        return false;
      case "caption":
        return true;
      case "chain":
        return selectable.some((o) => o.group === option.group && matchesQuery(o.label, query));
      case "member":
        return matchesQuery(option.label, query) || matchesQuery(option.group ?? "", query);
      default:
        return matchesQuery(option.label, query);
    }
  });
  return kept.filter(
    (option, index) => option.kind !== "caption" || kept[index + 1]?.kind === "rule",
  );
}

/**
 * Builds the Rule menu options with the row count of each, narrowed by a
 * case-insensitive substring query. It returns nothing while the workflow
 * holds no rules.
 */
export function ruleOptions(
  config: ConfigResponse | undefined,
  rows: readonly ActiveRow[],
  query: string,
): RuleOption[] {
  const rules = config?.dispatch.rules ?? [];
  if (config === undefined || rules.length === 0) {
    return [];
  }
  const options: RuleOption[] = [
    { kind: "any", value: "", label: "Any rule", count: rows.length, group: null },
  ];
  const chains = chainsOf(config);
  const chained = new Set(chains.flat());
  for (const chain of chains) {
    const [first] = chain;
    options.push({
      kind: "chain",
      value: `chain:${first}`,
      label: first,
      count: countRows(rows, (rule) => rule !== null && chain.includes(rule)),
      group: first,
    });
    for (const name of chain) {
      options.push({
        kind: "member",
        value: `rule:${name}`,
        label: name,
        count: countRows(rows, (rule) => rule === name),
        group: first,
      });
    }
  }
  const others: string[] = [];
  for (const name of rules.map((rule) => rule.name)) {
    if (name !== undefined && name !== "" && !chained.has(name) && !others.includes(name)) {
      others.push(name);
    }
  }
  for (const row of rows) {
    if (row.rule !== null && !chained.has(row.rule) && !others.includes(row.rule)) {
      others.push(row.rule);
    }
  }
  if (others.length > 0) {
    options.push({
      kind: "caption",
      value: "",
      label: chains.length > 0 ? "Other rules" : "Rules",
      count: 0,
      group: null,
    });
    for (const name of others) {
      options.push({
        kind: "rule",
        value: `rule:${name}`,
        label: name,
        count: countRows(rows, (rule) => rule === name),
        group: null,
      });
    }
  }
  if (rows.some((row) => row.rule === null)) {
    options.push({
      kind: "none",
      value: "none",
      label: "No rule",
      count: countRows(rows, (rule) => rule === null),
      group: null,
    });
  }
  return filterOptions(options, query);
}

/** Reports whether a search parameter value names a phase. */
export function isPhase(value: string | null): value is Phase {
  return PHASES.some((phase) => phase === value);
}

function matchesRule(row: ActiveRow, rule: string, config: ConfigResponse | undefined): boolean {
  if (rule === "none") {
    return row.rule === null;
  }
  if (rule.startsWith("rule:")) {
    return row.rule === rule.slice("rule:".length);
  }
  if (rule.startsWith("chain:")) {
    const first = rule.slice("chain:".length);
    const chain = config === undefined ? undefined : chainsOf(config).find((c) => c[0] === first);
    return row.rule !== null && chain !== undefined && chain.includes(row.rule);
  }
  return false;
}

/**
 * Keeps the rows a phase and a rule select. An unknown phase is ignored; a
 * rule no option carries matches nothing, so the list shows its empty state.
 */
export function filterRows(
  rows: readonly ActiveRow[],
  phase: string | null,
  rule: string | null,
  config: ConfigResponse | undefined,
): ActiveRow[] {
  return rows.filter(
    (row) =>
      (!isPhase(phase) || row.phase === phase) &&
      (rule === null || rule === "" || matchesRule(row, rule, config)),
  );
}

/** Counts rows per phase; pass rows already narrowed by the rule. */
export function chipCounts(rows: readonly ActiveRow[]): Record<Phase, number> {
  const counts: Record<Phase, number> = {
    running: 0,
    pull_request: 0,
    between_stages: 0,
    retry: 0,
    needs_attention: 0,
  };
  for (const row of rows) {
    counts[row.phase] += 1;
  }
  return counts;
}
