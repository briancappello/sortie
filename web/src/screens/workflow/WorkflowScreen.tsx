import { Link } from "react-router";

import { useStateQuery } from "@/api/queries";
import { fmtCount, fmtRate, fmtStampSec, fmtTokens } from "@/lib/format";

import { DispatchSection } from "./DispatchSection";
import { ReactionsSection } from "./ReactionsSection";
import { Block, Code, joinCode, msText, Row, Rows, Section, usageText } from "./workflow-text";

import type { ConfigResponse, TokenRateResponse, WorkflowHealthResponse } from "@/api/types.gen";
import type { ReactNode } from "react";

const HEADING = "font-heading text-h4 font-semibold text-ink";
const MESSAGE = "text-body-md text-ink-body";
const LINK =
  "text-link underline underline-offset-2 hover:text-link-hover focus-visible:outline-2 focus-visible:outline-focus-ring active:text-link-active";
const UNKNOWN = "unknown";
const NOT_SET = "not set";
const NONE = "none";

function stampText(at: string | null): string {
  return at === null ? UNKNOWN : fmtStampSec(at);
}

interface StandfirstProps {
  workflow: WorkflowHealthResponse | undefined;
}

function Standfirst({ workflow }: StandfirstProps) {
  if (workflow === undefined || workflow.checked_at === null) {
    return <p className={MESSAGE}>The configuration in effect.</p>;
  }
  const file = workflow.file ?? "The workflow file";
  if (workflow.status === "fail") {
    return (
      <p className={MESSAGE}>
        The configuration in effect. {file} has failed to load since{" "}
        {stampText(workflow.failing_since)}, so poll ticks dispatch nothing until it loads, and the
        configuration from its load at {stampText(workflow.last_success_at)} stays in effect; the
        error is on{" "}
        <Link to="/health" className={LINK}>
          Health
        </Link>
        .
      </p>
    );
  }
  return (
    <p className={MESSAGE}>
      The configuration in effect. {file} last loaded at {stampText(workflow.last_success_at)}.
    </p>
  );
}

function listText(values: readonly string[] | null): string {
  return values === null || values.length === 0 ? NONE : values.join(", ");
}

function stateText(value: string | undefined): string {
  return value === undefined || value === "" ? NOT_SET : value;
}

function sortedEntries(record: Record<string, number> | null): [string, number][] {
  return Object.entries(record ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

interface TrackerSectionProps {
  config: ConfigResponse;
}

function TrackerSection({ config }: TrackerSectionProps) {
  const { tracker, polling } = config;
  return (
    <Section title="Tracker">
      <Rows>
        <Row label="Kind">
          <Code>{tracker.kind}</Code>
        </Row>
        <Row label="Active states">{listText(tracker.active_states)}</Row>
        <Row label="In progress state">{stateText(tracker.in_progress_state)}</Row>
        <Row label="Handoff state">{stateText(tracker.handoff_state)}</Row>
        <Row label="No-change state">{stateText(tracker.no_change_state)}</Row>
        <Row label="Terminal states">{listText(tracker.terminal_states)}</Row>
        <Row label="Poll interval">every {msText(polling.interval_ms)}</Row>
      </Rows>
    </Section>
  );
}

function AgentSection({ config }: TrackerSectionProps) {
  const { agent } = config;
  const byState = sortedEntries(agent.max_concurrent_agents_by_state);
  return (
    <Section title="Agent">
      <Rows>
        <Row label="Kind">
          <Code>{agent.kind}</Code>
        </Row>
        <Row label="Token usage">{usageText(agent.usage_arrival)}</Row>
        <Row label="Max turns">{fmtCount(agent.max_turns)}</Row>
        <Row label="Stall timeout">
          {agent.stall_timeout_ms === null ? "off" : msText(agent.stall_timeout_ms)}
        </Row>
        <Row label="Retry backoff cap">{msText(agent.max_retry_backoff_ms)}</Row>
        <Row label="Session budget">
          {agent.max_sessions === null ? NONE : `${fmtCount(agent.max_sessions)} per issue`}
        </Row>
        <Row label="Token budget">
          {agent.max_tokens === null ? NONE : `${fmtTokens(agent.max_tokens)} per issue`}
        </Row>
        <Row label="Max concurrent agents">{fmtCount(agent.max_concurrent_agents)}</Row>
        {byState.length === 0 ? undefined : (
          <Row label="Per tracker state">
            {byState.map(([state, cap]) => `${state}: ${fmtCount(cap)}`).join(", ")}
          </Row>
        )}
      </Rows>
    </Section>
  );
}

function SshSection({ config }: TrackerSectionProps) {
  const hosts = config.worker.ssh_hosts ?? [];
  if (hosts.length === 0) {
    return undefined;
  }
  const perHost = config.worker.max_concurrent_agents_per_host;
  return (
    <Section title="SSH workers">
      <Rows>
        <Row label="Hosts">{joinCode(hosts, ", ")}</Row>
        <Row label="Agents per host">{perHost === null ? "no limit" : fmtCount(perHost)}</Row>
      </Rows>
    </Section>
  );
}

function rateText(rate: number | null, unset: string): string {
  return rate === null ? unset : `${fmtRate(rate)} per 1M`;
}

interface RateBlockProps {
  kind: string;
  rate: TokenRateResponse;
}

function RateBlock({ kind, rate }: RateBlockProps) {
  const unsetCache = rate.input_per_mtok === null ? NOT_SET : "not set, priced at the input rate";
  return (
    <Block
      name={kind}
      note={
        rate.complete ? undefined : (
          <>
            Not priced without <Code>input_per_mtok</Code> and <Code>output_per_mtok</Code>.
          </>
        )
      }
    >
      <Row label="Input">{rateText(rate.input_per_mtok, NOT_SET)}</Row>
      <Row label="Output">{rateText(rate.output_per_mtok, NOT_SET)}</Row>
      <Row label="Cache read">{rateText(rate.cache_read_per_mtok, unsetCache)}</Row>
      <Row label="Cache write">{rateText(rate.cache_write_per_mtok, unsetCache)}</Row>
    </Block>
  );
}

function TokenRatesSection({ config }: TrackerSectionProps) {
  const rates = config.token_rates;
  const kinds = rates === null ? [] : Object.keys(rates).sort();
  return (
    <Section title="Token rates">
      <p className="text-body-md text-ink-muted">
        An edit of token rates takes effect at the next restart.
      </p>
      {rates === null ? (
        <p className="text-body-md text-ink">Not set, so no cost is estimated.</p>
      ) : (
        kinds.map((kind) => <RateBlock key={kind} kind={kind} rate={rates[kind]} />)
      )}
    </Section>
  );
}

/** Renders Workflow: the configuration in effect as the state route serves it, with a standfirst that names a failed load of the file. */
export function WorkflowScreen(): ReactNode {
  const { data } = useStateQuery();
  if (data === undefined) {
    return <h1 className={HEADING}>Workflow</h1>;
  }
  const { config } = data;
  if (config === undefined) {
    return (
      <div className="flex flex-col gap-md">
        <h1 className={HEADING}>Workflow</h1>
        <p className={MESSAGE}>Sortie serves no configuration.</p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-md">
      <div className="flex flex-col gap-xs">
        <h1 className={HEADING}>Workflow</h1>
        <Standfirst workflow={data.health?.workflow} />
      </div>
      <div className="grid grid-cols-1 items-start gap-md min-[1024px]:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        <div className="flex min-w-0 flex-col gap-md">
          <DispatchSection
            dispatch={config.dispatch}
            agent={config.agent}
            handoffState={config.tracker.handoff_state}
          />
          <ReactionsSection reactions={config.reactions} />
        </div>
        <div className="flex min-w-0 flex-col gap-md">
          <TrackerSection config={config} />
          <AgentSection config={config} />
          <SshSection config={config} />
          <TokenRatesSection config={config} />
        </div>
      </div>
    </div>
  );
}
