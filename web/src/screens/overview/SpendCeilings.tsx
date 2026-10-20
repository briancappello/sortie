import { BudgetMeter } from "@/components/BudgetMeter";
import { fmtMoney, fmtUtcStamp } from "@/lib/format";

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
  const text = `${window.used_usd_complete ? "" : "at least "}${fmtMoney(used)} of ${fmtMoney(limit)}`;
  return (
    <div className="flex flex-col gap-xxs">
      <BudgetMeter
        name={name}
        line={{ used, limit, limitKnown: true }}
        format={fmtMoney}
        text={text}
      />
      <p className="text-body-sm text-ink-muted">
        {window.holds_dispatch ? "Holding dispatch, resets " : "Resets "}
        {fmtUtcStamp(resetsAt)}
      </p>
    </div>
  );
}

/** Renders the Spend section: one meter per configured instance spend window, daily first. */
export function SpendCeilings({ ceiling }: SpendCeilingsProps) {
  if (ceiling === undefined || (ceiling.daily === null && ceiling.monthly === null)) {
    return undefined;
  }
  return (
    <section className="flex flex-col gap-sm">
      <h2 className="font-heading text-h7 font-semibold text-ink">Spend</h2>
      {ceiling.daily === null ? undefined : (
        <SpendMeter name="Daily spend" window={ceiling.daily} />
      )}
      {ceiling.monthly === null ? undefined : (
        <SpendMeter name="Monthly spend" window={ceiling.monthly} />
      )}
    </section>
  );
}
