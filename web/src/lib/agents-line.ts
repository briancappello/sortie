import { plural } from "@/lib/format";

/**
 * Words how many agents run for the status card.
 *
 * A cap of one reads as "Agent running" or "Agent idle", never as a fraction;
 * an unserved cap leaves the bare count.
 */
export function agentsLine(running: number, cap: number | undefined): string {
  if (cap === 1) {
    return running > 0 ? "Agent running" : "Agent idle";
  }
  if (running === 0) {
    return "No agents running";
  }
  return cap === undefined
    ? `${plural(running, "agent", "agents")} running`
    : `${running} of ${plural(cap, "agent", "agents")} running`;
}
