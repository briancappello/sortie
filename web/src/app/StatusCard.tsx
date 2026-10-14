import { Link } from "react-router";

import { useStateQuery } from "@/api/queries";
import { StatusMark } from "@/components/StatusMark";
import { condition, conditionWord } from "@/lib/condition";
import { agentsLine } from "@/lib/agents-line";

import type { StateResponse } from "@/api/types.gen";

interface StatusSummaryProps {
  state: StateResponse;
}

function StatusSummary({ state }: StatusSummaryProps) {
  const current = condition(state);

  return (
    <>
      <span className="block font-medium">
        <StatusMark mark={current} label={conditionWord(current)} />
      </span>
      <span className="mt-xxs block pl-[calc(var(--size-icon)+var(--spacing-xs))] text-body-sm text-ink-muted [overflow-wrap:anywhere]">
        {agentsLine(state.counts.running, state.config?.agent.max_concurrent_agents)}
      </span>
    </>
  );
}

/** Renders the rail's link to Overview: the condition word with its mark, and how many agents run. */
export function StatusCard() {
  const { data } = useStateQuery();

  return (
    <Link
      to="/"
      className="mb-md block rounded-xl border border-line bg-canvas p-sm transition-colors duration-(--duration-feedback) hover:border-line-medium hover:bg-surface-raised focus-visible:-outline-offset-1 focus-visible:outline-2 focus-visible:outline-focus-ring"
    >
      {data === undefined ? (
        <span className="font-medium text-ink">Loading</span>
      ) : (
        <StatusSummary state={data} />
      )}
    </Link>
  );
}
