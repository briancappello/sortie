import { useStateQuery } from "@/api/queries";
import { ServedText } from "@/components/ServedText";
import { StatusMark } from "@/components/StatusMark";
import { fmtCount, fmtStampSec, parseInstant } from "@/lib/format";

import type { HealthCheckResponse, StateHealthResponse } from "@/api/types.gen";
import type { Mark } from "@/lib/run-status";
import type { ReactNode } from "react";

const HEADING = "font-heading text-h4 font-semibold text-ink";
const MESSAGE = "text-body-md text-ink-body";
const NOT_RUN = "Not run yet.";
const WORKFLOW_FAILED =
  "The latest load failed, so poll ticks dispatch nothing until the file loads, and the configuration from the last successful load stays in effect.";

function markOf(check: HealthCheckResponse): Mark {
  if (check.checked_at === null) {
    return "waiting";
  }
  return check.status === "fail" ? "failed" : "succeeded";
}

function stampText(at: string | null, whenNull: string): string {
  return at === null ? whenNull : fmtStampSec(at);
}

function nextPollText(nextPollAt: string | null, generatedAt: string): string {
  if (nextPollAt === null) {
    return "none scheduled";
  }
  const next = parseInstant(nextPollAt);
  const generated = parseInstant(generatedAt);
  return next !== null && generated !== null && next <= generated
    ? "due now"
    : fmtStampSec(nextPollAt);
}

interface FactProps {
  label: string;
  children: ReactNode;
}

function Fact({ label, children }: FactProps) {
  return (
    <>
      <dt className="text-ink-muted">{label}</dt>
      <dd className="text-ink [overflow-wrap:anywhere]">{children}</dd>
    </>
  );
}

interface NextPoll {
  at: string | null;
  generatedAt: string;
}

interface CheckRowProps {
  name: ReactNode;
  runLabel: string;
  check: HealthCheckResponse;
  /** The line under the name once the check has run. */
  line?: ReactNode;
  /** The line under the name before the check's first run. */
  unrunLine?: ReactNode;
  nextPoll?: NextPoll;
  errorCaption?: string;
}

function CheckRow({
  name,
  runLabel,
  check,
  line,
  unrunLine = NOT_RUN,
  nextPoll,
  errorCaption,
}: CheckRowProps) {
  const hasRun = check.checked_at !== null;
  const isFailing = check.status === "fail";
  const shownLine = hasRun ? line : unrunLine;
  const hasError = check.error !== undefined && check.error !== "";

  return (
    <li className="flex items-start gap-sm px-md py-sm">
      <span className="mt-xxs">
        <StatusMark mark={markOf(check)} />
      </span>
      <div className="flex min-w-0 flex-1 flex-col gap-xs">
        <h2 className="font-heading text-h7 font-semibold text-ink">{name}</h2>
        {shownLine === undefined ? undefined : (
          <p className="text-body-md text-ink-body">{shownLine}</p>
        )}
        {hasRun || nextPoll !== undefined ? (
          <dl className="grid grid-cols-[minmax(0,max-content)_minmax(0,1fr)] gap-x-md gap-y-xxs text-body-md max-sm:grid-cols-1 max-sm:gap-y-0">
            {check.checked_at === null ? undefined : (
              <>
                <Fact label={runLabel}>{fmtStampSec(check.checked_at)}</Fact>
                <Fact label="Last success">
                  {stampText(check.last_success_at, "none since Sortie started")}
                </Fact>
                <Fact label="Failures in a row">{fmtCount(check.consecutive_failures)}</Fact>
                {isFailing ? (
                  <Fact label="Failing since">{stampText(check.failing_since, "unknown")}</Fact>
                ) : undefined}
              </>
            )}
            {nextPoll === undefined ? undefined : (
              <Fact label="Next poll">{nextPollText(nextPoll.at, nextPoll.generatedAt)}</Fact>
            )}
          </dl>
        ) : undefined}
        {hasRun && hasError ? (
          <div className="flex flex-col gap-xxs">
            {errorCaption === undefined ? undefined : (
              <span className="text-label-caps font-medium text-ink-muted">{errorCaption}</span>
            )}
            <pre className="rounded-lg bg-surface-raised px-sm py-xs font-mono text-body-sm whitespace-pre-wrap text-ink [overflow-wrap:anywhere]">
              {check.error}
            </pre>
          </div>
        ) : undefined}
      </div>
    </li>
  );
}

interface ChecksProps {
  health: StateHealthResponse;
  generatedAt: string;
}

function Checks({ health, generatedAt }: ChecksProps) {
  const { workflow, preflight, poll, database, scm } = health;
  const sentence = <ServedText text={poll.summary_text} />;

  return (
    <ul className="divide-y divide-line overflow-hidden rounded-lg border border-line bg-canvas">
      <CheckRow
        name={workflow.file ?? "Workflow file"}
        runLabel="Last load"
        check={workflow}
        line={workflow.status === "fail" ? WORKFLOW_FAILED : undefined}
      />
      <CheckRow name="Dispatch preflight" runLabel="Last run" check={preflight} />
      <CheckRow
        name="Tracker poll"
        runLabel="Last poll"
        check={poll}
        line={sentence}
        unrunLine={sentence}
        nextPoll={{ at: poll.next_poll_at, generatedAt }}
        errorCaption="Last tracker error"
      />
      <CheckRow name="Database" runLabel="Last check" check={database} />
      {scm === null ? undefined : (
        <CheckRow
          name={
            <>
              Source control{" "}
              <code className="rounded bg-surface-raised px-xxs font-mono text-[0.9em]">
                {scm.provider}
              </code>
            </>
          }
          runLabel="Last read"
          check={scm}
        />
      )}
    </ul>
  );
}

/** Renders Health: one row per check of Sortie itself with its last run, last success, failures in a row, and error. */
export function HealthScreen(): ReactNode {
  const { data } = useStateQuery();
  if (data === undefined) {
    return <h1 className={HEADING}>Health</h1>;
  }

  return (
    <div className="flex flex-col gap-md">
      <h1 className={HEADING}>Health</h1>
      {data.health === undefined ? (
        <p className={MESSAGE}>Sortie serves no health checks.</p>
      ) : (
        <Checks health={data.health} generatedAt={data.generated_at} />
      )}
    </div>
  );
}
