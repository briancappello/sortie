import { fmtCount } from "@/lib/format";

import { Code, Section, selectionText, usageText } from "./workflow-text";

import type { ShapeAgent, ShapeDispatch, ShapeRule } from "@/api/types.gen";
import type { ReactNode } from "react";

const HEAD = "px-md py-sm text-left text-label-caps font-medium whitespace-nowrap text-ink-muted";
const CELL = "px-md py-sm whitespace-nowrap";
const CHAIN_HEADING = "font-heading text-body-lg font-semibold text-ink";

interface FallbackProps {
  dispatch: ShapeDispatch;
  agent: ShapeAgent;
}

function Fallback({ dispatch, agent }: FallbackProps) {
  if (dispatch.default !== null) {
    return (
      <>
        <Code>dispatch.default</Code>, agent <Code>{dispatch.default.agent_kind}</Code>, token usage{" "}
        {usageText(dispatch.default.usage_arrival)}
      </>
    );
  }
  return (
    <>
      agent <Code>{agent.kind}</Code>, token usage {usageText(agent.usage_arrival)}
    </>
  );
}

interface RulesTableProps {
  rules: readonly ShapeRule[];
}

function RulesTable({ rules }: RulesTableProps) {
  const hasNext = rules.some((rule) => rule.next !== undefined && rule.next !== "");

  return (
    <div className="max-w-full overflow-x-auto rounded-lg border border-line">
      <table className="w-full border-collapse text-body-md text-ink-body">
        <thead className="border-b border-line">
          <tr>
            <th scope="col" className={HEAD}>
              #
            </th>
            <th scope="col" className={HEAD}>
              Rule
            </th>
            <th scope="col" className={HEAD}>
              Selected by
            </th>
            {hasNext ? (
              <th scope="col" className={HEAD}>
                Next
              </th>
            ) : undefined}
            <th scope="col" className={HEAD}>
              Agent
            </th>
            <th scope="col" className={HEAD}>
              Token usage
            </th>
          </tr>
        </thead>
        <tbody>
          {rules.map((rule, index) => (
            <tr key={index} className="border-t border-line first:border-t-0">
              <td className={`${CELL} tabular-nums`}>{index + 1}</td>
              <td className={CELL}>
                {rule.name === undefined || rule.name === "" ? (
                  <span className="text-ink-muted">unnamed</span>
                ) : (
                  <Code>{rule.name}</Code>
                )}
              </td>
              <td className={CELL}>{selectionText(rule)}</td>
              {hasNext ? (
                <td className={CELL}>
                  {rule.next === undefined || rule.next === "" ? undefined : (
                    <Code>{rule.next}</Code>
                  )}
                </td>
              ) : undefined}
              <td className={CELL}>
                <Code>{rule.agent_kind}</Code>
              </td>
              <td className={CELL}>{usageText(rule.usage_arrival)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

interface ChainProps {
  title: string;
  stages: readonly string[];
  rules: readonly ShapeRule[];
  handoffState: string | undefined;
}

function Chain({ title, stages, rules, handoffState }: ChainProps) {
  return (
    <div className="flex flex-col gap-xs">
      <h3 className={CHAIN_HEADING}>{title}</h3>
      <ol className="ml-lg flex list-decimal flex-col gap-xxs text-body-md text-ink-body">
        {stages.map((name, index) => {
          const rule = rules.find((candidate) => candidate.name === name);
          return (
            <li key={index}>
              <Code>{name}</Code>
              {rule === undefined ? undefined : <> · {selectionText(rule)}</>}
            </li>
          );
        })}
      </ol>
      {handoffState === undefined || handoffState === "" ? undefined : (
        <p>Then hands off to {handoffState}.</p>
      )}
    </div>
  );
}

function hopLine(dispatch: ShapeDispatch): ReactNode {
  const hops = `At most ${fmtCount(dispatch.hop_ceiling)} automatic stage hops in a row per issue`;
  return dispatch.max_consecutive_hops === null ? (
    `${hops}, the default.`
  ) : (
    <>
      {hops}, set by <Code>dispatch.max_consecutive_hops</Code>.
    </>
  );
}

/** Props of {@link DispatchSection}. */
export interface DispatchSectionProps {
  dispatch: ShapeDispatch;
  agent: ShapeAgent;
  handoffState: string | undefined;
}

/** Renders Dispatch: the rules in served order, the fallback for an unselected issue, the stage chains, and the hop ceiling. */
export function DispatchSection({ dispatch, agent, handoffState }: DispatchSectionProps) {
  const rules = dispatch.rules ?? [];
  const chains = dispatch.chains ?? [];
  const hasCatchAll = rules.some((rule) => rule.selected_by === "catch_all");

  return (
    <Section title="Dispatch">
      {rules.length === 0 ? (
        <p className="text-body-md text-ink">
          No dispatch rules: every issue runs on <Fallback dispatch={dispatch} agent={agent} />.
        </p>
      ) : (
        <>
          <RulesTable rules={rules} />
          {hasCatchAll ? undefined : (
            <p className="text-body-md text-ink-body">
              Issues no rule selects run on <Fallback dispatch={dispatch} agent={agent} />.
            </p>
          )}
        </>
      )}
      {chains.map((stages, index) => (
        <Chain
          key={index}
          title={chains.length === 1 ? "Stage Chain" : `Stage Chain (${index + 1})`}
          stages={stages}
          rules={rules}
          handoffState={handoffState}
        />
      ))}
      {chains.length === 0 ? undefined : (
        <p className="text-body-md text-ink-body">{hopLine(dispatch)}</p>
      )}
    </Section>
  );
}
