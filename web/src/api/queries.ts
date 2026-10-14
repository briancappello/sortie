import { QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";

import { ApiError, getIssue, getRuns, getState, getStats } from "@/api/client";
import { setServerOffset } from "@/lib/server-clock";

import type { IssueDetailResponse, Report, RunsResponse, StateResponse } from "@/api/types.gen";
import type { UseQueryResult } from "@tanstack/react-query";

/** The one cache of server data in a tab; a change signal, not a timer, decides when data is stale. */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: Infinity, refetchOnWindowFocus: true },
    mutations: { retry: false },
  },
});

/** Builds the cache keys; `midnight` is the start of the reader's day in RFC 3339 UTC. */
export const queryKeys = {
  state: () => ["state"] as const,
  issue: (identifier: string) => ["issue", identifier] as const,
  ended: (midnight: string) => ["ended", midnight] as const,
  tally: (midnight: string) => ["tally", midnight] as const,
  firstRun: () => ["first-run"] as const,
};

const ENDED_PAGE_SIZE = 200;

async function fetchEnded(midnight: string, signal: AbortSignal): Promise<RunsResponse[]> {
  const pages: RunsResponse[] = [];
  let cursor: string | undefined;
  do {
    const page = await getRuns({ ended_since: midnight, limit: ENDED_PAGE_SIZE, cursor }, signal);
    pages.push(page);
    cursor = page.next_cursor ?? undefined;
  } while (cursor !== undefined);
  return pages;
}

async function fetchHasRun(signal: AbortSignal): Promise<boolean> {
  try {
    const page = await getRuns({ limit: 1 }, signal);
    return (page.runs ?? []).length > 0;
  } catch (error) {
    if (error instanceof ApiError && error.status === 503 && error.code === "history_unavailable") {
      return false;
    }
    throw error;
  }
}

async function fetchState(signal: AbortSignal): Promise<StateResponse> {
  const state = await getState(signal);
  setServerOffset(state.generated_at);
  return state;
}

/** Reads the runtime state and fixes the server clock offset from every response. */
export function useStateQuery(): UseQueryResult<StateResponse> {
  return useQuery({ queryKey: queryKeys.state(), queryFn: ({ signal }) => fetchState(signal) });
}

/** Reads one issue; it stays idle while no issue is shown. */
export function useIssueQuery(identifier: string | undefined): UseQueryResult<IssueDetailResponse> {
  return useQuery({
    queryKey: queryKeys.issue(identifier ?? ""),
    queryFn: ({ signal }) => getIssue(identifier ?? "", signal),
    enabled: identifier !== undefined,
  });
}

/** Reads every page of runs that ended since the instant. */
export function useEndedQuery(midnight: string): UseQueryResult<RunsResponse[]> {
  return useQuery({
    queryKey: queryKeys.ended(midnight),
    queryFn: ({ signal }) => fetchEnded(midnight, signal),
  });
}

/** Reads the run statistics since the instant. */
export function useTallyQuery(midnight: string): UseQueryResult<Report> {
  return useQuery({
    queryKey: queryKeys.tally(midnight),
    queryFn: ({ signal }) => getStats({ since: midnight }, signal),
  });
}

/**
 * Tells whether no run is on record: `true` while none is, `undefined` until
 * the first answer. Once a run exists the query is disabled for the rest of
 * the page's life, so no change signal refetches it.
 */
export function useFirstRunQuery(): boolean | undefined {
  const client = useQueryClient();
  const hasRun = client.getQueryData<boolean>(queryKeys.firstRun()) === true;
  const { data } = useQuery({
    queryKey: queryKeys.firstRun(),
    queryFn: ({ signal }) => fetchHasRun(signal),
    enabled: !hasRun,
  });
  return data === undefined ? undefined : !data;
}
