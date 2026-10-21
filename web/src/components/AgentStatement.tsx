import { ServedText } from "@/components/ServedText";
import { cn } from "@/lib/cn";

/** Props of {@link AgentStatement}. */
export interface AgentStatementProps {
  text: string;
  className?: string;
}

/**
 * Renders what the agent wrote with its declared outcome, in curly quotes that mark
 * the words as the agent's own. The text reaches the page only through
 * {@link ServedText}.
 */
export function AgentStatement({ text, className }: AgentStatementProps) {
  return (
    <span className={cn("block text-ink-body wrap-anywhere", className)}>
      {"“"}
      <ServedText text={text} />
      {"”"}
    </span>
  );
}
