import { Tooltip as BaseTooltip } from "@base-ui/react/tooltip";

import type { ReactNode } from "react";

/** Props of {@link Tooltip}. */
export type TooltipProps = BaseTooltip.Root.Props;

/** Holds the open state of a tooltip, which opens on hover and on keyboard focus of its trigger. */
export function Tooltip(props: TooltipProps) {
  return <BaseTooltip.Root {...props} />;
}

/** Props of {@link TooltipTrigger}. */
export type TooltipTriggerProps = BaseTooltip.Trigger.Props;

/** Renders the element the tooltip describes; pass `render` to put the trigger on an existing control. */
export function TooltipTrigger(props: TooltipTriggerProps) {
  return <BaseTooltip.Trigger {...props} />;
}

/** Props of {@link TooltipPopup}. */
export interface TooltipPopupProps {
  children: ReactNode;
}

/** Renders the short text floating above its trigger. */
export function TooltipPopup({ children }: TooltipPopupProps) {
  return (
    <BaseTooltip.Portal>
      <BaseTooltip.Positioner side="top" sideOffset={4} className="z-(--z-tooltip)">
        <BaseTooltip.Popup className="max-w-72 rounded bg-surface-inverse px-xs py-xxs text-body-sm text-on-dark shadow-lg transition-opacity duration-(--duration-enter-small) data-ending-style:opacity-0 data-starting-style:opacity-0">
          {children}
        </BaseTooltip.Popup>
      </BaseTooltip.Positioner>
    </BaseTooltip.Portal>
  );
}

/** Props of {@link Hint}. */
export interface HintProps {
  hint: ReactNode;
  children: ReactNode;
}

/**
 * Renders text with a dotted underline whose tooltip carries the detail the
 * screen leaves out. It takes keyboard focus, so the tooltip opens without a
 * pointer too.
 */
export function Hint({ hint, children }: HintProps) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <span
            tabIndex={0}
            className="cursor-help underline decoration-line-strong decoration-dotted underline-offset-4 transition-colors duration-(--duration-feedback) outline-none hover:decoration-ink-body focus-visible:rounded-xs focus-visible:outline-2 focus-visible:outline-focus-ring"
          />
        }
      >
        {children}
      </TooltipTrigger>
      <TooltipPopup>{hint}</TooltipPopup>
    </Tooltip>
  );
}
