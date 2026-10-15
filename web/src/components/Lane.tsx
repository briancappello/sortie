import { Meter } from "@base-ui/react/meter";

import { CircleAlertFilledIcon, XIcon } from "@/components/icons";
import { markWord } from "@/components/StatusMark";
import { cn } from "@/lib/cn";
import { ruleLabel } from "@/lib/labels";

import type { LaneStation, StagePosition, StageProgress, StationState } from "@/lib/lane";
import type { Mark } from "@/lib/run-status";

/** The longest chain the meter draws one segment per stage for; a longer chain draws one bar. */
const SEGMENT_LIMIT = 12;

const MARKER: Record<Mark, string> = {
  running: "size-3.5 bg-track-done",
  current: "size-3.5 bg-track-done",
  waiting: "size-3.5 border-3 border-track-done bg-canvas",
  retry: "size-3.5 border-2 border-ink-muted bg-canvas",
  held: "size-3.5 border-[1.5px] border-dashed border-ink-muted bg-canvas",
  stopped: "size-3.5 border-[1.5px] border-dashed border-ink-muted bg-canvas",
  needs: "size-4 text-warning-fill",
  handed: "size-3.5 bg-track-done",
  succeeded: "size-3.5 bg-track-done",
  failed: "size-3.5 border-2 border-error-fill bg-canvas",
  degraded: "size-3.5 border-2 border-error-fill bg-canvas",
  clear: "size-3.5 bg-track-done",
};

const DOT: Record<StationState, string> = {
  passed: "border-track-done bg-track-done",
  current: "border-track-done bg-track-done",
  escalated: "border-track-done bg-canvas",
  waiting: "border-dashed border-track-done bg-canvas",
  ahead: "border-track-todo bg-canvas",
  idle: "border-track-todo bg-canvas",
};

function xOf(index: number, count: number): number {
  return 5 + (index * 90) / Math.max(1, count - 1);
}

/** Props of {@link StageMeter}. */
export interface StageMeterProps {
  position: StagePosition;
  progress: StageProgress;
  /** Draws the current stage in the attention color. */
  needs: boolean;
  /** Whether the issue stands at the agent station; elsewhere the meter shows only the stages done. */
  atAgent: boolean;
}

/**
 * Draws the stages of a chain as segments, or as one bar past twelve stages.
 * At the agent station the caption names the stage and its place; elsewhere it
 * counts the stages done.
 */
export function StageMeter({ position, progress, needs, atAgent }: StageMeterProps) {
  const { k, n, done } = progress;
  const focus = atAgent ? progress.focus : null;
  const caption = atAgent ? `${ruleLabel(position.current)} · ${k} of ${n}` : `${done} of ${n}`;
  const stages = Array.from({ length: n }, (_, index) => index + 1);
  const currentTone = needs ? "bg-warning-fill" : "bg-ink";

  return (
    <div className="flex flex-col items-center gap-xxs">
      <Meter.Root value={done} max={n} aria-label={caption} className="w-full">
        {n <= SEGMENT_LIMIT ? (
          <div aria-hidden="true" className="flex h-1 gap-px">
            {stages.map((stage) => (
              <span
                key={stage}
                className={cn(
                  "min-w-0 flex-1 rounded-[1px]",
                  stage <= done
                    ? "bg-track-done"
                    : focus?.kind === "current" && stage === k
                      ? currentTone
                      : "bg-track-todo",
                )}
              />
            ))}
          </div>
        ) : (
          <Meter.Track className="relative block h-1 overflow-hidden rounded-full bg-track-todo">
            <Meter.Indicator className="block h-full bg-track-done" />
            {focus?.kind === "current" ? (
              <span
                aria-hidden="true"
                className={cn("absolute inset-y-0", currentTone)}
                style={{ left: `${((k - 1) / n) * 100}%`, width: `${100 / n}%` }}
              />
            ) : undefined}
          </Meter.Track>
        )}
      </Meter.Root>
      <p aria-hidden="true" className="whitespace-nowrap text-body-sm text-ink-body tabular-nums">
        {caption}
      </p>
    </div>
  );
}

/** Props of {@link Lane}. */
export interface LaneProps {
  stations: LaneStation[];
  position: StagePosition | null;
  progress: StageProgress | null;
  /** Whether a state row decides where the issue stands; a finished issue shows when it ended. */
  live: boolean;
  /** Draws the track alone, without the card and the station names, as the peek does. */
  compact?: boolean;
}

/**
 * Draws the stations an issue can stand at as one track, the current station
 * marked with its time above it. The stage meter sits under the agent station,
 * which holds all agent work whatever stage the issue is on. Positions are
 * shares of the width, so the track never scrolls sideways.
 */
export function Lane({ stations, position, progress, live, compact = false }: LaneProps) {
  const count = stations.length;
  const current = stations.findIndex((station) => station.state === "current");
  const standing = current < 0 ? undefined : stations[current];
  const agent = stations.findIndex((station) => station.kind === "agent");
  const hasMeter = position !== null && progress !== null && agent >= 0;
  const reached = current < 0 ? 0 : xOf(current, count) - xOf(0, count);
  const headWord =
    standing === undefined
      ? undefined
      : !live
        ? `ended ${standing.time ?? ""}`.trim()
        : standing.mark === null
          ? undefined
          : markWord(standing.mark);

  const track = (
    <div
      aria-hidden="true"
      className={cn(
        "relative",
        compact ? "h-10" : "h-18",
        hasMeter && (compact ? "mb-md" : "mb-xl"),
      )}
    >
      <div
        className="absolute top-1/2 h-px bg-track-todo"
        style={{ left: `${xOf(0, count)}%`, right: `${100 - xOf(count - 1, count)}%` }}
      />
      <div
        className="absolute top-[calc(50%-1px)] h-[3px] rounded-full bg-track-done"
        style={{ left: `${xOf(0, count)}%`, width: `${reached}%` }}
      />
      {stations.map((station, index) => {
        const left = `${xOf(index, count)}%`;
        const isCurrent = index === current;
        return (
          <div key={station.key}>
            {isCurrent ? (
              <CurrentMarker
                station={station}
                left={left}
                edge={index === count - 1 ? "end" : index === 0 ? "start" : null}
              />
            ) : (
              <StationDot station={station} left={left} />
            )}
            {compact ? undefined : (
              <span
                className={cn(
                  "absolute top-[calc(50%+12px)] -translate-x-1/2 whitespace-nowrap text-body-sm",
                  isCurrent ? "font-semibold text-ink" : "text-ink-body max-md:hidden",
                )}
                style={{ left }}
              >
                {station.label}
              </span>
            )}
          </div>
        );
      })}
      {hasMeter ? (
        <div
          className={cn(
            "absolute -translate-x-1/2",
            compact ? "top-[calc(50%+16px)] w-14" : "top-[calc(50%+32px)] w-24",
          )}
          style={{ left: `${xOf(agent, count)}%` }}
        >
          <StageMeter
            position={position}
            progress={progress}
            needs={stations[agent]?.mark === "needs"}
            atAgent={current === agent}
          />
        </div>
      ) : undefined}
    </div>
  );

  const list = (
    <ol className="sr-only" aria-label="Stations">
      {stations.map((station) => (
        <li key={station.key}>
          {[station.label, station.state === "idle" ? undefined : station.state, station.time]
            .filter((part) => part !== undefined && part !== null)
            .join(", ")}
        </li>
      ))}
      {hasMeter ? (
        <li>{`${ruleLabel(position.current)}, ${progress.k} of ${progress.n}`}</li>
      ) : undefined}
    </ol>
  );

  if (compact) {
    return (
      <div className="min-w-0">
        {list}
        {track}
      </div>
    );
  }
  return (
    <section className="min-w-0 rounded-xl border border-line bg-canvas px-lg py-md max-md:px-md">
      <h2 className="mb-xxs flex items-baseline gap-sm text-body-sm text-ink-muted">
        <span className="font-semibold text-ink-body">Pipeline</span>
        {headWord === undefined ? undefined : <span>{headWord}</span>}
      </h2>
      {list}
      {track}
    </section>
  );
}

interface StationMarkProps {
  station: LaneStation;
  left: string;
}

interface CurrentMarkerProps extends StationMarkProps {
  /** Which end of the track the station sits at, so its time stays inside the card. */
  edge: "start" | "end" | null;
}

function CurrentMarker({ station, left, edge }: CurrentMarkerProps) {
  const mark = station.mark ?? "current";
  return (
    <>
      <span
        className={cn(
          "absolute top-1/2 z-(--z-raised) grid -translate-1/2 place-items-center rounded-full shadow-[0_0_0_2px_var(--color-canvas)]",
          MARKER[mark],
        )}
        style={{ left }}
        title={`${station.label}: ${markWord(mark)}`}
      >
        {mark === "needs" ? <CircleAlertFilledIcon className="size-4" /> : undefined}
      </span>
      {station.time === null ? undefined : (
        <span
          className={cn(
            "absolute bottom-[calc(50%+10px)] whitespace-nowrap text-body-sm text-ink-body tabular-nums",
            edge === "end" ? "-translate-x-full" : edge === "start" ? "" : "-translate-x-1/2",
          )}
          style={{ left }}
        >
          {station.time}
        </span>
      )}
    </>
  );
}

function StationDot({ station, left }: StationMarkProps) {
  if (station.state === "escalated") {
    return (
      <span
        className="absolute top-1/2 -translate-1/2 text-ink-body"
        style={{ left }}
        title={`${station.label}: escalated`}
      >
        <XIcon className="size-3.5" />
      </span>
    );
  }
  return (
    <span
      className={cn(
        "absolute top-1/2 size-[11px] -translate-1/2 rounded-full border-[1.5px]",
        DOT[station.state],
      )}
      style={{ left }}
      title={station.state === "waiting" ? `${station.label}: waiting` : station.label}
    />
  );
}
