import { useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  use,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";

import { useStateQuery } from "@/api/queries";
import { now } from "@/lib/server-clock";
import { connectChangeStream } from "@/stream/client";

import type { ReactNode } from "react";

import type { StreamMessage, StreamStatus } from "@/stream/core";

const STATE_KEY_ROOT = "state";

let streamStatus: StreamStatus = "connecting";
let streamStarted = false;
const statusListeners = new Set<() => void>();
const changeListeners = new Set<() => void>();

function handleStreamMessage(message: StreamMessage): void {
  if (message.kind === "status") {
    streamStatus = message.status;
    for (const listener of statusListeners) {
      listener();
    }
    return;
  }
  for (const listener of changeListeners) {
    listener();
  }
}

/**
 * Connects the tab to the change stream; later calls do nothing. Call it
 * before React renders so the first change signal is not missed.
 */
export function startLiveStream(): void {
  if (streamStarted) {
    return;
  }
  streamStarted = true;
  connectChangeStream(handleStreamMessage);
}

function subscribeStatus(listener: () => void): () => void {
  statusListeners.add(listener);
  return () => statusListeners.delete(listener);
}

function getStreamStatus(): StreamStatus {
  return streamStatus;
}

/** What the shell and the screens read about live updates. */
export interface LiveValue {
  paused: boolean;
  /** Flips the pause and returns the new value; resuming replays a change signal that arrived while paused. */
  togglePause: () => boolean;
  streamStatus: StreamStatus;
  /** The toast text once a state response carries another `version` than the first one; `null` before and after dismissal. */
  versionNotice: string | null;
  dismissVersionNotice: () => void;
}

const LiveContext = createContext<LiveValue | undefined>(undefined);

/** Provides the pause, the stream status, and the version notice, and refetches queries on every change signal. */
export function LiveProvider({ children }: LiveProviderProps) {
  const client = useQueryClient();
  const { data } = useStateQuery();
  const [paused, setPaused] = useState(false);
  const [versionNotice, setVersionNotice] = useState<string | null>(null);
  const pausedRef = useRef(false);
  const pendingChange = useRef(false);
  const firstVersion = useRef<string | null>(null);
  const noticedVersion = useRef<string | null>(null);
  const status = useSyncExternalStore(subscribeStatus, getStreamStatus);

  const handleChange = useCallback(() => {
    if (pausedRef.current) {
      pendingChange.current = true;
      return;
    }
    void client.invalidateQueries({ refetchType: "none" });
    void client.refetchQueries({ queryKey: [STATE_KEY_ROOT] });
    if (document.visibilityState !== "hidden") {
      void client.refetchQueries({
        type: "active",
        stale: true,
        predicate: (query) => query.queryKey[0] !== STATE_KEY_ROOT,
      });
    }
  }, [client]);

  useEffect(() => {
    startLiveStream();
    changeListeners.add(handleChange);
    return () => {
      changeListeners.delete(handleChange);
    };
  }, [handleChange]);

  const version = data?.version;
  useEffect(() => {
    if (version === undefined) {
      return;
    }
    if (firstVersion.current === null) {
      firstVersion.current = version;
    } else if (version !== firstVersion.current && version !== noticedVersion.current) {
      noticedVersion.current = version;
      setVersionNotice(`Sortie ${version} is running. Reload to update the dashboard.`);
    }
  }, [version]);

  const togglePause = useCallback(() => {
    const next = !pausedRef.current;
    pausedRef.current = next;
    setPaused(next);
    if (!next && pendingChange.current) {
      pendingChange.current = false;
      handleChange();
    }
    return next;
  }, [handleChange]);

  const dismissVersionNotice = useCallback(() => setVersionNotice(null), []);

  const value = useMemo<LiveValue>(
    () => ({
      paused,
      togglePause,
      streamStatus: status,
      versionNotice,
      dismissVersionNotice,
    }),
    [paused, togglePause, status, versionNotice, dismissVersionNotice],
  );

  return <LiveContext value={value}>{children}</LiveContext>;
}

interface LiveProviderProps {
  children: ReactNode;
}

/**
 * Reads the live-update value.
 *
 * @throws When used outside `LiveProvider`.
 */
export function useLive(): LiveValue {
  const value = use(LiveContext);
  if (value === undefined) {
    throw new Error("useLive must be used inside LiveProvider");
  }
  return value;
}

/**
 * Returns the value as it was when `held` turned true for as long as `held`
 * stays true, else the live value. Pass one object bundling everything a view
 * renders so the whole view holds still together.
 */
export function useHold<T>(value: T, held: boolean): T {
  const [snapshot, setSnapshot] = useState<{ value: T } | null>(null);
  if (held && snapshot === null) {
    setSnapshot({ value });
  } else if (!held && snapshot !== null) {
    setSnapshot(null);
  }
  return held && snapshot !== null ? snapshot.value : value;
}

/** Returns the server-clock time in epoch milliseconds, refreshed every second and frozen while paused. */
export function useNow(): number {
  const { paused } = useLive();
  const [tick, setTick] = useState(now);

  useEffect(() => {
    if (paused) {
      return;
    }
    const id = setInterval(() => setTick(now()), 1_000);
    return () => clearInterval(id);
  }, [paused]);

  return tick;
}
