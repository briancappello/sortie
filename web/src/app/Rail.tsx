import { NavLink } from "react-router";

import { useStateQuery } from "@/api/queries";
import { BrandMark } from "@/app/BrandMark";
import { keyTip } from "@/app/key-tips";
import { modifierLabel } from "@/app/keys";
import { StatusCard } from "@/app/StatusCard";
import { CircleHelpIcon, SearchIcon } from "@/components/icons";
import { ThemeMenu } from "@/components/ThemeMenu";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/cn";
import { fmtVersion, pluralVerb } from "@/lib/format";

import type { StateResponse } from "@/api/types.gen";
import type { IconProps } from "@/components/icons";
import type { ReactNode } from "react";

/**
 * One screen the shell can open. `key` is the letter pressed after `G`, `path`
 * is relative to the application's base path, and screens with the same `group`
 * sit together, with a divider between groups in ascending order. The screen at
 * path `/` is Overview, which carries the attention badge.
 */
export interface ScreenEntry {
  key: string;
  label: string;
  path: string;
  group: number;
  icon: (props: IconProps) => ReactNode;
}

const OVERVIEW_PATH = "/";

function hasCriticalItem(state: StateResponse): boolean {
  return (state.attention ?? []).some(
    (item) => item.acknowledged_at === null && item.severity === "critical",
  );
}

function groupsOf(screens: ScreenEntry[]): ScreenEntry[][] {
  const numbers = [...new Set(screens.map((screen) => screen.group))].sort((a, b) => a - b);
  return numbers.map((group) => screens.filter((screen) => screen.group === group));
}

interface AttentionBadgeProps {
  state: StateResponse;
}

function AttentionBadge({ state }: AttentionBadgeProps) {
  const count = state.counts.attention;
  if (count <= 0) {
    return undefined;
  }
  const attentionPhrase = pluralVerb(count, ["item", "items"], ["needs", "need"]);
  const tone = hasCriticalItem(state)
    ? "bg-error-fill text-on-warning-fill"
    : "bg-warning-fill text-on-warning-fill";
  return (
    <span
      title={`${attentionPhrase.subject} ${attentionPhrase.verb} attention`}
      className={cn(
        "ml-auto inline-grid h-(--size-kbd) min-w-(--size-kbd) place-items-center rounded-full px-xs text-body-sm font-medium tabular-nums",
        tone,
      )}
    >
      {count}
    </span>
  );
}

/** Props of {@link Rail}. */
export interface RailProps {
  screens: ScreenEntry[];
  onOpenPalette: () => void;
  onOpenShortcuts: () => void;
}

/** Renders the dark navigation rail: brand, status card, one entry per registered screen, and the foot controls. */
export function Rail({ screens, onOpenPalette, onOpenShortcuts }: RailProps) {
  const { data } = useStateQuery();
  const groups = groupsOf(screens);

  return (
    <aside
      data-theme="dark"
      aria-label="Sortie navigation"
      className="sticky top-0 hidden h-screen flex-col bg-surface px-sm py-lg text-ink [--ghost-hover:var(--color-item-hover-panel)] [--ghost-pressed:var(--color-item-selected-panel)] md:flex"
    >
      <div className="flex items-center gap-sm px-sm pb-md">
        <BrandMark />
        <span className="font-heading text-h7 font-semibold">Sortie</span>
        {data === undefined ? undefined : (
          <span
            title={data.version}
            className="ml-auto whitespace-nowrap text-body-sm text-ink-muted"
          >
            {fmtVersion(data.version)}
          </span>
        )}
      </div>
      <StatusCard />
      <nav aria-label="Main" className="min-h-0 overflow-y-auto">
        {groups.map((group, index) => (
          <div key={group[0].group}>
            {index > 0 ? <div role="separator" className="mx-sm mb-md h-px bg-line" /> : undefined}
            <div className="mb-md grid gap-xxs">
              {group.map((screen) => (
                <NavLink
                  key={screen.path}
                  to={screen.path}
                  end
                  className="flex h-(--size-nav-item) items-center gap-sm rounded-lg px-sm text-ink-body transition-colors duration-(--duration-feedback) hover:bg-item-hover-panel hover:text-ink focus-visible:-outline-offset-1 focus-visible:outline-2 focus-visible:outline-focus-ring active:bg-item-selected-panel aria-[current=page]:bg-item-selected-panel aria-[current=page]:font-medium aria-[current=page]:text-ink"
                >
                  <screen.icon />
                  {screen.label}
                  {screen.path === OVERVIEW_PATH && data !== undefined ? (
                    <AttentionBadge state={data} />
                  ) : undefined}
                </NavLink>
              ))}
            </div>
          </div>
        ))}
      </nav>
      <div className="mt-auto grid gap-xxs pt-md">
        <Button
          variant="ghost"
          className="w-full justify-start font-regular"
          onClick={onOpenPalette}
          title={keyTip("Commands", modifierLabel(), "K")}
        >
          <SearchIcon />
          Commands
        </Button>
        <Button
          variant="ghost"
          className="w-full justify-start font-regular"
          onClick={onOpenShortcuts}
          title={keyTip("Shortcuts", "?")}
        >
          <CircleHelpIcon />
          Shortcuts
        </Button>
        <ThemeMenu />
      </div>
    </aside>
  );
}
