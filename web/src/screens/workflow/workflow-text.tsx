import { Fragment } from "react";

import { fmtDuration } from "@/lib/format";

import type { ShapeMatch, ShapePriority, ShapeRule } from "@/api/types.gen";
import type { ReactNode } from "react";

/** Props of {@link Code}. */
export interface CodeProps {
  children: ReactNode;
}

/** Renders a configured name or pattern in the monospace face. */
export function Code({ children }: CodeProps) {
  return (
    <code className="rounded bg-surface-raised px-xxs font-mono text-[0.9em] [overflow-wrap:anywhere]">
      {children}
    </code>
  );
}

/** Joins nodes with a separator text, keying each so the result renders as siblings. */
export function joinNodes(nodes: readonly ReactNode[], separator: string): ReactNode {
  return nodes.map((node, index) => (
    <Fragment key={index}>
      {index === 0 ? undefined : separator}
      {node}
    </Fragment>
  ));
}

/** Renders each value in the monospace face, joined by a separator text. */
export function joinCode(values: readonly string[], separator: string): ReactNode {
  return values.map((value, index) => (
    <Fragment key={index}>
      {index === 0 ? undefined : separator}
      <Code>{value}</Code>
    </Fragment>
  ));
}

function priorityClause({ op, value, values }: ShapePriority): ReactNode {
  switch (op) {
    case "eq":
      return `priority ${value}`;
    case "in":
      return `priority ${(values ?? []).join(" or ")}`;
    case "lt":
      return `priority below ${value}`;
    case "lte":
      return `priority ${value} or below`;
    case "gt":
      return `priority above ${value}`;
    case "gte":
      return `priority ${value} or above`;
    default:
      return `priority ${op} ${value}`;
  }
}

function matchClauses(match: ShapeMatch): ReactNode[] {
  const clauses: ReactNode[] = [];
  if (match.labels !== undefined && match.labels.length > 0) {
    clauses.push(<>label {joinCode(match.labels, " or ")}</>);
  }
  if (match.issue_type !== undefined && match.issue_type.length > 0) {
    clauses.push(`issue type ${match.issue_type.join(" or ")}`);
  }
  if (match.priority !== undefined) {
    clauses.push(priorityClause(match.priority));
  }
  if (match.identifier !== undefined && match.identifier.length > 0) {
    clauses.push(<>identifier {joinCode(match.identifier, " or ")}</>);
  }
  if (match.assignee !== undefined && match.assignee.length > 0) {
    clauses.push(`assignee ${match.assignee.join(" or ")}`);
  }
  if (match.title !== undefined && match.title.length > 0) {
    clauses.push(`title ${match.title.map((phrase) => `"${phrase}"`).join(" or ")}`);
  }
  return clauses;
}

/** Words how a dispatch rule selects an issue; a selection kind the client has no wording for reads as served. */
export function selectionText(rule: Pick<ShapeRule, "selected_by" | "stage" | "match">): ReactNode {
  switch (rule.selected_by) {
    case "stage":
      return (
        <>
          stage label <Code>{rule.stage}</Code>
        </>
      );
    case "catch_all":
      return "every other issue";
    case "match": {
      const clauses = rule.match === undefined ? [] : matchClauses(rule.match);
      return clauses.length === 0 ? rule.selected_by : joinNodes(clauses, " and ");
    }
    default:
      return rule.selected_by;
  }
}

/** Words whether an agent reports token usage; an arrival the client has no wording for reads "not declared". */
export function usageText(arrival: string): string {
  switch (arrival) {
    case "incremental":
    case "turn_end":
      return "reported";
    case "none":
      return "not reported";
    default:
      return "not declared";
  }
}

/** Words a configured span in milliseconds. */
export function msText(ms: number): string {
  return fmtDuration(ms / 1000);
}

/** Words where a reaction escalates; an escalation the client has no wording for reads as served. */
export function escalationText(escalation: string, label: string | undefined): ReactNode {
  switch (escalation) {
    case "label":
      return label === undefined ? (
        "label"
      ) : (
        <>
          label <Code>{label}</Code>
        </>
      );
    case "comment":
      return "comment";
    case "none":
      return "none";
    default:
      return escalation;
  }
}

/** Props of {@link Section}. */
export interface SectionProps {
  title: string;
  children: ReactNode;
}

/** Renders one titled section of the Workflow screen. */
export function Section({ title, children }: SectionProps) {
  return (
    <section className="flex min-w-0 flex-col gap-sm rounded-lg border border-line bg-canvas p-md">
      <h2 className="font-heading text-h7 font-semibold text-ink">{title}</h2>
      {children}
    </section>
  );
}

/** Props of {@link Rows}. */
export interface RowsProps {
  children: ReactNode;
}

/** Lays out {@link Row}s as a label column and a value column. */
export function Rows({ children }: RowsProps) {
  return (
    <dl className="grid grid-cols-[minmax(0,max-content)_minmax(0,1fr)] gap-x-md gap-y-xs text-body-md max-sm:grid-cols-1 max-sm:gap-y-0">
      {children}
    </dl>
  );
}

/** Props of {@link Row}. */
export interface RowProps {
  label: string;
  children: ReactNode;
}

/** Renders one label and its value. */
export function Row({ label, children }: RowProps) {
  return (
    <>
      <dt className="text-ink-muted max-sm:mt-xs">{label}</dt>
      <dd className="text-ink [overflow-wrap:anywhere]">{children}</dd>
    </>
  );
}

/** Props of {@link Block}. */
export interface BlockProps {
  name: string;
  /** A line below the rows. */
  note?: ReactNode;
  children: ReactNode;
}

/** Renders a group of rows headed by a configured name, such as a reaction key or an agent kind. */
export function Block({ name, note, children }: BlockProps) {
  return (
    <div className="flex flex-col gap-xs border-t border-line pt-sm">
      <h3 className="text-body-md font-semibold text-ink">
        <Code>{name}</Code>
      </h3>
      <Rows>{children}</Rows>
      {note === undefined ? undefined : <p className="text-body-md text-ink-muted">{note}</p>}
    </div>
  );
}
