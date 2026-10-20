import type { ReactNode } from "react";

/** Props of {@link Code}. */
export interface CodeProps {
  children: ReactNode;
}

/** Renders a configured name or pattern in the monospace face. */
export function Code({ children }: CodeProps) {
  return (
    <code className="rounded bg-surface-raised px-xxs font-mono text-[0.9em] wrap-anywhere">
      {children}
    </code>
  );
}
