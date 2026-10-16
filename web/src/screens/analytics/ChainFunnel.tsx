import { cn } from "@/lib/cn";
import { fmtCount, fmtDuration } from "@/lib/format";

import { CELL, HEAD, HEAD_RIGHT } from "./GroupTable";

import type { ConfigResponse, Funnel, FunnelStage } from "@/api/types.gen";
import type { ReactNode } from "react";

const MONO = "font-mono text-body-sm";
const MUTED = "text-ink-muted";
const RIGHT = `${CELL} text-right`;
const ENTRY_TAG =
  "inline-block border border-line-medium rounded px-xxs text-label-caps font-medium uppercase text-ink-muted";

const REASON_WORDS: Record<string, string> = {
  run_failed: "failed run",
  needs_person: "needs a person",
  budget_held: "budget held",
  operator_stopped: "stopped by an operator",
  verdict_withheld: "verdict withheld",
};

function reasonWord(reason: string): string {
  return REASON_WORDS[reason] ?? reason;
}

function handoffTargets(config: ConfigResponse | undefined): string | undefined {
  const { handoff_state: handoff, no_change_state: noChange } = config?.tracker ?? {};
  if (handoff === undefined || handoff === "") {
    return undefined;
  }
  return noChange === undefined || noChange === "" ? handoff : `${handoff} or ${noChange}`;
}

interface CountCellProps {
  value: number;
}

// A zero is muted so the figures that are not zero carry the table.
function CountCell({ value }: CountCellProps) {
  return <td className={cn(RIGHT, value === 0 && MUTED)}>{fmtCount(value)}</td>;
}

interface StageRowProps {
  stage: FunnelStage;
  index: number;
  widest: number;
  targets: string | undefined;
}

function StageRow({ stage, index, widest, targets }: StageRowProps) {
  const { arrivals, outcomes } = stage;
  const reasons = stage.no_hop_reasons ?? [];
  const reasonsTitle =
    reasons.length > 1
      ? reasons.map((entry) => `${fmtCount(entry.passes)} ${reasonWord(entry.reason)}`).join(", ")
      : undefined;

  return (
    <tr className="border-t border-line first:border-t-0">
      <td className={cn(RIGHT, MUTED)}>{index + 1}</td>
      <td className={CELL}>
        <span className={MONO}>{stage.rule}</span>
        {stage.entry ? (
          <>
            {" "}
            <span className={ENTRY_TAG}>entry</span>
          </>
        ) : undefined}
      </td>
      <CountCell value={stage.runs} />
      <td className={CELL}>
        <span className="inline-grid grid-cols-[48px_auto] items-center gap-xs">
          <span aria-hidden="true" className="block h-2 bg-transparent">
            <span
              className="block h-full rounded-r bg-viz-1"
              style={{ width: `${(stage.entered / widest) * 100}%` }}
            />
          </span>
          {fmtCount(stage.entered)}
        </span>
      </td>
      <td className={RIGHT}>{arrivals.label === 0 ? undefined : fmtCount(arrivals.label)}</td>
      {stage.last ? (
        <td className={RIGHT} title={targets === undefined ? undefined : `moved to ${targets}`}>
          {fmtCount(outcomes.handed_off)} <span className={MUTED}>handed off</span>
        </td>
      ) : (
        <CountCell value={outcomes.advanced} />
      )}
      <CountCell value={outcomes.ceiling} />
      <CountCell value={outcomes.hop_failed} />
      <td className={cn(CELL, outcomes.no_hop === 0 && MUTED)} title={reasonsTitle}>
        {fmtCount(outcomes.no_hop)}
        {reasons[0] === undefined ? undefined : (
          <>
            {" "}
            <span className={MUTED}>{reasonWord(reasons[0].reason)}</span>
          </>
        )}
      </td>
      <CountCell value={outcomes.in_stage} />
      <td className={cn(RIGHT, stage.median_seconds === null && MUTED)}>
        {fmtDuration(stage.median_seconds)}
      </td>
    </tr>
  );
}

function headingOf(stages: readonly FunnelStage[]): ReactNode {
  const entries = stages.filter((stage) => stage.entry);
  return (
    <>
      Chain from{" "}
      {entries.map((stage, index) => (
        <span key={stage.rule}>
          {index > 0 ? " or " : undefined}
          <span className={MONO}>{stage.rule}</span>
        </span>
      ))}
    </>
  );
}

/** Props of {@link ChainFunnel}. */
export interface ChainFunnelProps {
  funnel: Funnel;
  config: ConfigResponse | undefined;
}

/** Renders one chain definition of the report: a row per stage with the passes that reached it and how each one left it. */
export function ChainFunnel({ funnel, config }: ChainFunnelProps) {
  const stages = funnel.stages ?? [];
  const widest = Math.max(1, ...stages.map((stage) => stage.entered));
  const targets = handoffTargets(config);

  return (
    <section className="flex flex-col gap-sm">
      <h2 className="font-heading text-h7 font-semibold text-ink">{headingOf(stages)}</h2>
      <div className="max-w-full overflow-x-auto rounded-lg border border-line bg-canvas">
        <table className="w-full border-collapse text-body-md text-ink-body">
          <thead className="border-b border-line">
            <tr>
              <th scope="col" className={cn(HEAD_RIGHT, "w-[36px]")}>
                #
              </th>
              <th scope="col" className={HEAD}>
                Stage
              </th>
              <th
                scope="col"
                className={HEAD_RIGHT}
                title="Finished runs of the rule, retries and pull request fixes included"
              >
                Runs
              </th>
              <th scope="col" className={HEAD} title="Passes through the stage, each counted once">
                Entered
              </th>
              <th
                scope="col"
                className={HEAD_RIGHT}
                title="Passes that arrived by a stage label a person put on the issue"
              >
                By label
              </th>
              <th
                scope="col"
                className={HEAD_RIGHT}
                title={`Passes that hopped to the next stage; on the last stage, ${targets === undefined ? "handed off" : `moved to ${targets}`}`}
              >
                Advanced
              </th>
              <th
                scope="col"
                className={HEAD_RIGHT}
                title="Hops refused by dispatch.max_consecutive_hops"
              >
                Ceiling
              </th>
              <th scope="col" className={HEAD_RIGHT}>
                Hop failed
              </th>
              <th
                scope="col"
                className={HEAD}
                title="The run failed, stopped for a person or at a budget, was stopped by an operator, or had its handoff withheld, so no hop was due"
              >
                No hop
              </th>
              <th
                scope="col"
                className={HEAD_RIGHT}
                title="Passes whose run is going or retrying now"
              >
                In stage
              </th>
              <th scope="col" className={HEAD_RIGHT}>
                Median
              </th>
            </tr>
          </thead>
          <tbody>
            {stages.map((stage, index) => (
              <StageRow
                key={stage.rule}
                stage={stage}
                index={index}
                widest={widest}
                targets={targets}
              />
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
