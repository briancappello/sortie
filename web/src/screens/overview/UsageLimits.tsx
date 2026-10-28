import { BudgetMeter } from "@/components/BudgetMeter";
import { Parts } from "@/components/Parts";
import { fmtAgo, fmtAmount, fmtPercent, fmtUtcStamp, parseInstant } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";

type KindUsage = NonNullable<StateResponse["rate_limits"]>[string];
type UsageWindow = NonNullable<KindUsage["windows"]>[number];

const HOUR_MS = 3_600_000;

const STATUS_WORDS: Readonly<Record<string, string>> = {
  allowed: "Under the limit",
  warning: "Near the limit",
  exhausted: "Usage limit reached",
};

function whenText(at: string, now: number): string {
  const instant = parseInstant(at);
  return instant !== null && now - instant < HOUR_MS ? fmtAgo(at, now) : fmtUtcStamp(at);
}

function isDrawn(window: UsageWindow, now: number): boolean {
  const resetsAt = window.resets_at === null ? null : parseInstant(window.resets_at);
  return resetsAt === null || resetsAt > now;
}

interface UsageWindowBlockProps {
  window: UsageWindow;
  memberReportedAt: string;
  now: number;
}

function UsageWindowBlock({ window, memberReportedAt, now }: UsageWindowBlockProps) {
  const {
    label,
    used_percent: used,
    amount,
    status,
    resets_at: resetsAt,
    reported_at: reportedAt,
  } = window;
  const statusWords = status === null ? undefined : STATUS_WORDS[status];
  const hasBar = amount !== null || used !== null;
  const isBarAlert = hasBar && (status === "warning" || status === "exhausted");
  const parts: string[] = [];
  if (isBarAlert && statusWords !== undefined) {
    parts.push(statusWords);
  }
  parts.push(resetsAt === null ? "Reset time not reported" : `Resets ${fmtUtcStamp(resetsAt)}`);
  if (reportedAt !== memberReportedAt) {
    parts.push(`Reported ${whenText(reportedAt, now)}`);
  }
  return (
    <div className="flex flex-col gap-xxs">
      {amount !== null ? (
        <BudgetMeter
          name={label}
          line={{ used: amount.used, limit: amount.limit, limitKnown: true }}
          format={fmtAmount}
          text={`${fmtAmount(amount.used)} of ${fmtAmount(amount.limit)} ${amount.unit}`}
        />
      ) : used === null ? (
        <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-sm text-body-md">
          <span className="text-ink">{label}</span>
          <span className="text-ink-body">{statusWords ?? "Status not reported"}</span>
        </div>
      ) : (
        <BudgetMeter
          name={label}
          line={{ used, limit: 100, limitKnown: true }}
          format={(n) => fmtPercent(n / 100)}
          text={`${fmtPercent(used / 100)} used`}
        />
      )}
      <p className="text-body-sm text-ink-muted">
        <Parts parts={parts} />
      </p>
    </div>
  );
}

interface UsageKindBlockProps {
  kind: string;
  usage: KindUsage | undefined;
  now: number;
}

function UsageKindBlock({ kind, usage, now }: UsageKindBlockProps) {
  const windows = (usage?.windows ?? []).filter((window) => isDrawn(window, now));
  return (
    <div className="flex flex-col gap-sm">
      <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-sm text-body-md">
        <h3 className="text-ink">{kind}</h3>
        <span className="text-body-sm text-ink-muted">
          {usage === undefined ? "Not reported" : `Reported ${whenText(usage.reported_at, now)}`}
        </span>
      </div>
      {usage === undefined
        ? undefined
        : windows.map((window, index) => (
            <UsageWindowBlock
              key={index}
              window={window}
              memberReportedAt={usage.reported_at}
              now={now}
            />
          ))}
    </div>
  );
}

/** Props of {@link UsageLimits}. */
export interface UsageLimitsProps {
  rateLimits: StateResponse["rate_limits"];
  /** The listed agent kinds, one block each whether or not it reported; empty while `config` is not served. */
  kinds: readonly string[];
  /** Overview's server-corrected clock in epoch milliseconds. */
  now: number;
}

/** Renders the Usage limits section: one block per listed agent kind with the provider usage windows it last reported. */
export function UsageLimits({ rateLimits, kinds, now }: UsageLimitsProps) {
  if (kinds.length === 0) {
    return undefined;
  }
  return (
    <section className="flex flex-col gap-sm">
      <h2 className="font-heading text-h7 font-semibold text-ink">Usage limits</h2>
      {kinds.map((kind) => (
        <UsageKindBlock key={kind} kind={kind} usage={rateLimits[kind]} now={now} />
      ))}
    </section>
  );
}
