import { fmtCount } from "@/lib/format";

import { Block, Code, escalationText, msText, Row, Section } from "./workflow-text";

import type {
  ShapeAutoMerge,
  ShapeBotReview,
  ShapeCIFailure,
  ShapeLabelCommands,
  ShapeMergeCompletion,
  ShapeMergeConflicts,
  ShapeReactions,
  ShapeReviewComments,
} from "@/api/types.gen";
import type { ReactNode } from "react";

function yesNo(value: boolean): string {
  return value ? "yes" : "no";
}

function triageText(triage: boolean): string {
  return triage ? "runs" : "off";
}

interface EscalationRowProps {
  reaction: { escalation: string; escalation_label?: string };
}

function EscalationRow({ reaction }: EscalationRowProps) {
  return (
    <Row label="Escalation">{escalationText(reaction.escalation, reaction.escalation_label)}</Row>
  );
}

function CIFailure({ reaction }: { reaction: ShapeCIFailure }) {
  return (
    <Block name="ci_failure">
      <Row label="CI fixes">{fmtCount(reaction.max_retries)}</Row>
      <Row label="Triage command">{triageText(reaction.triage)}</Row>
      <EscalationRow reaction={reaction} />
    </Block>
  );
}

function ReviewComments({ reaction }: { reaction: ShapeReviewComments }) {
  return (
    <Block name="review_comments">
      <Row label="Review fix runs">{fmtCount(reaction.max_continuation_turns)}</Row>
      <Row label="Debounce">{msText(reaction.debounce_ms)}</Row>
      <Row label="Checks every">{msText(reaction.poll_interval_ms)}</Row>
      <Row label="Triage command">{triageText(reaction.triage)}</Row>
      <EscalationRow reaction={reaction} />
    </Block>
  );
}

function BotReview({ reaction }: { reaction: ShapeBotReview }) {
  const accounts = reaction.bot_usernames ?? [];
  return (
    <Block name="bot_review">
      <Row label="Bot review fix runs">{fmtCount(reaction.max_continuation_turns)}</Row>
      <Row label="Bot accounts">{accounts.length === 0 ? "none" : accounts.join(", ")}</Row>
      <Row label="Checks every">{msText(reaction.poll_interval_ms)}</Row>
      <Row label="Triage command">{triageText(reaction.triage)}</Row>
      <EscalationRow reaction={reaction} />
    </Block>
  );
}

function MergeConflicts({ reaction }: { reaction: ShapeMergeConflicts }) {
  return (
    <Block name="merge_conflicts">
      <Row label="Rebase runs per conflict">{fmtCount(reaction.max_retries)}</Row>
      <Row label="Checks every">{msText(reaction.poll_interval_ms)}</Row>
      <Row label="Triage command">{triageText(reaction.triage)}</Row>
      <EscalationRow reaction={reaction} />
    </Block>
  );
}

function AutoMerge({ reaction }: { reaction: ShapeAutoMerge }) {
  return (
    <Block name="auto_merge">
      <Row label="Strategy">{reaction.strategy}</Row>
      <Row label="Waits for CI">{yesNo(reaction.require_ci)}</Row>
      <Row label="Deletes the branch">{yesNo(reaction.delete_branch)}</Row>
      <Row label="Merge calls">
        {reaction.max_retries === null ? "no limit" : fmtCount(reaction.max_retries)}
      </Row>
      <Row label="Checks every">{msText(reaction.poll_interval_ms)}</Row>
      <EscalationRow reaction={reaction} />
    </Block>
  );
}

function MergeCompletion({ reaction }: { reaction: ShapeMergeCompletion }) {
  return (
    <Block name="merge_completion">
      <Row label="Moves the issue to">{reaction.target_state}</Row>
      <Row label="Failed tracker moves">{fmtCount(reaction.max_retries)}</Row>
      <Row label="Checks every">{msText(reaction.poll_interval_ms)}</Row>
      <EscalationRow reaction={reaction} />
    </Block>
  );
}

function LabelCommands({ reaction }: { reaction: ShapeLabelCommands }) {
  return (
    <Block name="label_commands">
      {reaction.review_label === undefined || reaction.review_label === "" ? undefined : (
        <Row label="Review label">
          <Code>{reaction.review_label}</Code>
        </Row>
      )}
      {reaction.fix_label === undefined || reaction.fix_label === "" ? undefined : (
        <Row label="Fix label">
          <Code>{reaction.fix_label}</Code>
        </Row>
      )}
      <Row label="Checks every">{msText(reaction.poll_interval_ms)}</Row>
    </Block>
  );
}

/** Props of {@link ReactionsSection}. */
export interface ReactionsSectionProps {
  reactions: ShapeReactions;
}

/** Renders Reactions: one block per configured pull request reaction, in a fixed order, under the restart note. */
export function ReactionsSection({ reactions }: ReactionsSectionProps) {
  const blocks: ReactNode[] = [];
  if (reactions.ci_failure !== undefined) {
    blocks.push(<CIFailure key="ci_failure" reaction={reactions.ci_failure} />);
  }
  if (reactions.review_comments !== undefined) {
    blocks.push(<ReviewComments key="review_comments" reaction={reactions.review_comments} />);
  }
  if (reactions.bot_review !== undefined) {
    blocks.push(<BotReview key="bot_review" reaction={reactions.bot_review} />);
  }
  if (reactions.merge_conflicts !== undefined) {
    blocks.push(<MergeConflicts key="merge_conflicts" reaction={reactions.merge_conflicts} />);
  }
  if (reactions.auto_merge !== undefined) {
    blocks.push(<AutoMerge key="auto_merge" reaction={reactions.auto_merge} />);
  }
  if (reactions.merge_completion !== undefined) {
    blocks.push(<MergeCompletion key="merge_completion" reaction={reactions.merge_completion} />);
  }
  if (reactions.label_commands !== undefined) {
    blocks.push(<LabelCommands key="label_commands" reaction={reactions.label_commands} />);
  }

  return (
    <Section title="Reactions">
      <p className="text-body-md text-ink-muted">
        An edit of reactions takes effect at the next restart, except the CI fixes and escalation of{" "}
        <Code>ci_failure</Code>, which take effect when the file loads.
      </p>
      {blocks.length === 0 ? (
        <p className="text-body-md text-ink">No pull request reactions are configured.</p>
      ) : (
        blocks
      )}
    </Section>
  );
}
