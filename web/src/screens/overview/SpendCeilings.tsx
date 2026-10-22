import { BudgetMeter } from "@/components/BudgetMeter";
import { costTitle } from "@/lib/figures";
import { fmtMoney, fmtMoneyAtLeast, fmtUtcStamp } from "@/lib/format";

import type { SpendCeilingResponse, SpendWindowResponse } from "@/api/types.gen";

/** Props of {@link SpendCeilings}. */
export interface SpendCeilingsProps {
  ceiling: SpendCeilingResponse | undefined;
}

interface SpendMeterProps {
  name: string;
  window: SpendWindowResponse;
}

function SpendMeter({ name, window }: SpendMeterProps) {
  const { used_usd: used, budget_usd: limit, resets_at: resetsAt } = window;
  const isComplete = window.used_usd_complete;
  const text = `${isComplete ? fmtMoney(used) : fmtMoneyAtLeast(used)} of ${fmtMoney(limit)}`;
  return (
    <div className="flex flex-col gap-xxs">
      <BudgetMeter
        name={name}
        line={{ used, limit, limitKnown: true }}
        format={fmtMoney}
        text={text}
        title={costTitle(isComplete ? "estimated" : "partial", used)}
      />
      <p className="text-body-sm text-ink-muted">
        {window.holds_dispatch ? "Holding dispatch, resets " : "Resets "}
        {fmtUtcStamp(resetsAt)}
      </p>
    </div>
  );
}

// Record forces a label for every window the API serves, so a new member fails tsc here.
// Key order is the render order.
const SPEND_LABELS: Record<keyof SpendCeilingResponse, string> = {
  daily: "Daily spend",
  weekly: "Weekly spend",
  monthly: "Monthly spend",
};

// Object.keys widens to string[]; the keys are exactly those of SPEND_LABELS.
const SPEND_WINDOWS = Object.keys(SPEND_LABELS) as (keyof SpendCeilingResponse)[];

/** Renders the Spend section: one meter per configured instance spend window, shortest window first. */
export function SpendCeilings({ ceiling }: SpendCeilingsProps) {
  if (ceiling === undefined) {
    return undefined;
  }
  const meters = SPEND_WINDOWS.flatMap((key) => {
    const window = ceiling[key];
    return window === null
      ? []
      : [<SpendMeter key={key} name={SPEND_LABELS[key]} window={window} />];
  });
  if (meters.length === 0) {
    return undefined;
  }
  return (
    <section className="flex flex-col gap-sm">
      <h2 className="font-heading text-h7 font-semibold text-ink">Spend</h2>
      {meters}
    </section>
  );
}
