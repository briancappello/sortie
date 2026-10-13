package sentence

import (
	"fmt"
	"maps"
	"slices"
	"text/template"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// fallbackText stands in for a sentence whose template failed to render.
const fallbackText = "Status not available"

// parts holds the template sources of one table entry: short and long for a
// state sentence, text for an outcome, condition and text for a next outcome.
type parts struct {
	short     string
	long      string
	text      string
	condition string
}

// Fragments shared by several templates.
const (
	ofLimit      = "{{if .Limit}} of {{count .Limit}}{{end}}"
	stagePlace   = "{{if .Stage}} stage {{.Stage}} ({{count .Pos}} of {{count .Len}}){{end}}"
	onStage      = "{{if .Stage}} on stage {{.Stage}}{{end}}"
	onStagePlace = "{{if .Stage}} on stage {{.Stage}} ({{count .Pos}} of {{count .Len}}){{end}}"
	iteration    = "{{if .Iteration}}, iteration {{count .Iteration}}{{end}}"
	nextCheck    = "{{.Check}} {{due .Next}}"
	usedCount    = "{{if .CountLabel}} {{.CountLabel}}: {{count .Used}}{{if .HasMax}} of {{count .Max}}{{end}}.{{end}}"
	ciProgress   = "{{if .HasCI}}: {{count .Pending}} pending, {{count .Passing}} passing{{end}}"
	failingList  = "{{if .FailingChecks}}{{list .FailingChecks}}{{else}}{{plural .FailingN \"failing check\" \"failing checks\"}}{{end}}"
	commentsOf   = "{{if .HasComments}}: {{plural .Actionable \"actionable comment\" \"actionable comments\"}}, {{count .New}} new{{end}}"
	newComments  = "{{plural .New \"new review comment\" \"new review comments\"}}"
	watched      = "{{if .Label}}the `{{.Label}}` label{{else}}a label command{{end}}"
	parkingLabel = "Parking label: `{{.Label}}`{{if .Unseen}} (not yet seen on the issue){{end}}."
	inNext       = "{{if .Window}} in the next {{duration .Window}}{{end}}"
	heldAt       = "{{if .TokenHold}}token budget of {{tokens .Budget}}{{else}}session budget ({{count .Sessions}} of {{count .MaxSessions}} sessions used){{end}}"
	ofMax        = "{{if .Max}} of {{count .Max}}{{end}}"
	posture      = "{{if eq .Escalation \"label\"}}applies `{{.EscalationLabel}}` to {{.Key}}" +
		"{{else if eq .Escalation \"comment\"}}comments on {{.Key}}" +
		"{{else}}sends `escalation.{{.ConfigKey}}` only to the notification destinations that subscribe to it{{end}}"
	afterAhead = "{{if eq .Ahead \"retry\"}} after {{retry .AheadTrigger .AheadRunInline .AheadAttempt}} ends" +
		"{{else if eq .Ahead \"run\"}} after the current run ends{{end}}"
)

var table = map[string]parts{
	"run_work": {
		short: "Turn {{count .Turn}}" + onStage,
		long:  "{{.Key}} is running" + stagePlace + ", turn {{count .Turn}}.",
	},
	"run_starting": {
		short: "Starting, no turn yet",
		long:  "{{.Key}} is starting" + stagePlace + "; no turn has begun yet.",
	},
	"run_self_review": {
		short: "Self-review" + iteration + onStage,
		long:  "{{.Key}} is in self-review" + onStagePlace + iteration + ".",
	},
	"run_ci_fix": {
		short: "Fixing CI on #{{.PR}}, fix {{count .Attempt}}" + ofLimit,
		long:  "{{.Key}} is fixing CI on PR #{{.PR}}, fix {{count .Attempt}}" + ofLimit + ".",
	},
	"run_merge_conflict": {
		short: "Resolving the merge conflict on #{{.PR}}, rebase {{count .Attempt}}" + ofLimit,
		long:  "{{.Key}} is resolving the merge conflict on PR #{{.PR}}, rebase {{count .Attempt}}" + ofLimit + ".",
	},
	"run_review_fix": {
		short: "Answering review comments on #{{.PR}}, fix {{count .Attempt}}" + ofLimit,
		long:  "{{.Key}} is answering review comments on PR #{{.PR}}, fix {{count .Attempt}}" + ofLimit + ".",
	},
	"run_bot_review_fix": {
		short: "Answering bot review comments on #{{.PR}}, fix {{count .Attempt}}" + ofLimit,
		long:  "{{.Key}} is answering bot review comments on PR #{{.PR}}, fix {{count .Attempt}}" + ofLimit + ".",
	},
	"run_label_review": {
		short: "Reviewing #{{.PR}}{{if .Label}} for `{{.Label}}`{{end}}",
		long:  "{{.Key}} is reviewing PR #{{.PR}}{{if .Label}}, as the `{{.Label}}` label asked{{end}}.",
	},
	"run_label_fix": {
		short: "Fixing #{{.PR}}{{if .Label}} for `{{.Label}}`{{end}}",
		long:  "{{.Key}} is fixing PR #{{.PR}}{{if .Label}}, as the `{{.Label}}` label asked{{end}}.",
	},

	"retry_paused": {
		short: "Retry {{count .Attempt}} paused: {{.Reason}}",
		long:  "{{.Key}}'s retry {{count .Attempt}} is paused: {{.Reason}}; next check {{due .Due}}.",
	},
	"retry_due_now": {
		short: "Retry {{count .Attempt}} due now",
		long:  "{{.Key}}'s retry {{count .Attempt}} is due now.",
	},
	"retry_error": {
		short: "Retry {{count .Attempt}} {{due .Due}} after an error",
		long:  "{{.Key}}'s last run ended with an error{{if .Error}}: {{.Error}}{{end}}. Retry {{count .Attempt}} {{due .Due}}.",
	},
	"retry_continuation": {
		short: "Next run {{due .Due}}: the issue is still active",
		long:  "{{.Key}}'s last run ended cleanly and the issue is still active; next run {{due .Due}}.",
	},
	"retry_stall": {
		short: "Retry {{count .Attempt}} {{due .Due}}: the agent stalled",
		long:  "{{.Key}}'s last run stalled: the agent sent no event within the stall timeout. Retry {{count .Attempt}} {{due .Due}}.",
	},
	"retry_timer": {
		short: "Retry {{count .Attempt}} {{due .Due}}: {{.Reason}}",
		long:  "{{.Key}}'s retry came due and was not dispatched: {{.Reason}}. Retry {{count .Attempt}} {{due .Due}}.",
	},
	"retry_ci_fix": {
		short: "CI fix run {{due .Due}}",
		long:  "{{.Key}}: CI failed on its pull request; fix run {{due .Due}}.",
	},
	"retry_reaction": {
		short: "{{.Run}} {{due .Due}}",
		long:  "{{.Key}}: {{.RunInline}} {{due .Due}}.",
	},
	"retry_generic": {
		short: "Retry {{count .Attempt}} {{due .Due}}",
		long:  "{{.Key}}: retry {{count .Attempt}} {{due .Due}}.",
	},

	"held_budget_tokens": {
		short: "Held: token budget reached",
		long:  "{{.Key}} is held at its token budget{{if .HasBudget}} of {{tokens .Budget}}{{end}}. Tokens used: {{if .AtLeast}}at least {{end}}{{tokens .Used}}.",
	},
	"held_budget_sessions": {
		short: "Held: session budget reached",
		long:  "{{.Key}} is held at its session budget" + ofLimit + ". Sessions used: {{count .Sessions}}.",
	},

	"parked_agent_blocked": {
		short: "Parked: the agent needs a person",
		long:  "{{.Key}} is parked: the agent stopped because it cannot go on without a person. " + parkingLabel,
	},
	"parked_handoff_absence": {
		short: "Parked: no evidence of work",
		long:  "{{.Key}} is parked: its runs ended without evidence of work as many times in a row as the limit allows. " + parkingLabel,
	},

	"held_escalated_ci": {
		short: "CI escalated on #{{.PR}}",
		long:  "{{.Key}}: Sortie escalated failing CI on PR #{{.PR}}; it keeps watching the pull request." + usedCount,
	},
	"held_escalated_reaction": {
		short: "{{.KindInitial}} escalated on #{{.PR}}",
		long:  "{{.Key}}: Sortie escalated {{.KindInline}} on PR #{{.PR}}." + usedCount,
	},
	"wait_ci": {
		short: "Waiting on CI for #{{.PR}}" + ciProgress,
		long:  "{{.Key}} is waiting on CI for PR #{{.PR}}" + ciProgress + "; " + nextCheck + "." + usedCount,
	},
	"wait_ci_passing": {
		short: "CI passing on #{{.PR}}",
		long:  "{{.Key}}: CI passes on PR #{{.PR}} ({{count .Passing}} passing); " + nextCheck + "." + usedCount,
	},
	"wait_ci_failing": {
		short: "CI failing on #{{.PR}}",
		long:  "{{.Key}}: CI fails on PR #{{.PR}}: " + failingList + "; " + nextCheck + "." + usedCount,
	},
	"wait_review_debounce": {
		short: newComments + " on #{{.PR}}; next check {{due .Next}}",
		long:  "{{.Key}} has " + newComments + " on PR #{{.PR}} ({{count .Actionable}} actionable) and waits for the comments to settle; next check {{due .Next}}." + usedCount,
	},
	"wait_review": {
		short: "Waiting on review of #{{.PR}}",
		long:  "{{.Key}} is waiting on review of PR #{{.PR}}" + commentsOf + "; " + nextCheck + "." + usedCount,
	},
	"wait_bot": {
		short: "Waiting on bot review of #{{.PR}}",
		long:  "{{.Key}} is waiting on bot review of PR #{{.PR}}{{if .Bots}} by {{.Bots}}{{end}}" + commentsOf + "; " + nextCheck + "." + usedCount,
	},
	"wait_merge": {
		short: "Auto-merge waiting on #{{.PR}}",
		long:  "{{.Key}}: auto-merge waits for PR #{{.PR}} to meet its merge conditions{{if .Strategy}} ({{.Strategy}} strategy){{end}}; " + nextCheck + "." + usedCount,
	},
	"merge_conflict_found": {
		short: "Merge conflict on #{{.PR}}",
		long:  "{{.Key}}: PR #{{.PR}} has a merge conflict; " + nextCheck + "." + usedCount,
	},
	"wait_merge_conflict": {
		short: "Watching #{{.PR}} for merge conflicts",
		long:  "{{.Key}}: Sortie watches PR #{{.PR}} for merge conflicts; {{.Mergeability}}; " + nextCheck + "." + usedCount,
	},
	"wait_merge_completion": {
		short: "Waiting for #{{.PR}} to merge",
		long:  "{{.Key}} waits{{if .Handoff}} in {{.Handoff}}{{end}} for PR #{{.PR}} to merge{{if .Target}}, then Sortie moves it to {{.Target}}{{end}}; " + nextCheck + "." + usedCount,
	},
	"wait_label_command": {
		short: "Watching #{{.PR}} for " + watched,
		long:  "{{.Key}}: Sortie watches PR #{{.PR}} for " + watched + "; " + nextCheck + ".",
	},
	"wait_reaction": {
		short: "Waiting on #{{.PR}}",
		long:  "{{.Key}} waits on a pull request reaction for PR #{{.PR}}; " + nextCheck + ".",
	},

	"stage_wait": {
		short: "Between stages: {{.Source}} to {{.Target}}",
		long:  "{{.Key}} moved from stage {{.Source}} to stage {{.Target}}, and Sortie added `{{.Label}}`; {{.Target}} starts when a poll tick reads that label on the issue.",
	},

	"ended_handed": {
		short: "Handed off to {{.State}}",
		long:  "{{.Key}} was handed off: Sortie moved it to {{.State}}.",
	},
	"ended_not_advanced": {
		short: "Handed off to {{.State}}; stage {{.Target}} not reached",
		long:  "{{.Key}} was handed off to {{.State}} because the hop to stage {{.Target}} was not made: {{.Why}}.",
	},
	"ended_merged": {
		short: "Merged #{{.PR}}",
		long:  "{{.Key}} ended merged: {{.HowBefore}}{{.PR}}{{.HowAfter}}.",
	},
	"ended_done": {
		short: "Reached {{.State}}",
		long:  "{{.Key}} reached the tracker state {{.State}}.",
	},
	"ended_none": {
		short: "Not active; no ending recorded",
		long:  "{{.Key}} is not active, and Sortie recorded no ending for its latest work.",
	},

	"next_if_run_succeeds": {condition: "If the run succeeds"},
	"next_if_run_fails":    {condition: "If the run fails"},
	"next_if_tokens_reach": {condition: "If {{.Key}}'s token use reaches {{tokens .Budget}}"},
	"next_when_retry_due":  {condition: "When {{retry .Trigger .RunInline .Attempt}} comes due"},
	"next_when_label_read": {condition: "When a poll tick reads `{{.Label}}` on {{.Key}}"},
	"next_if_ci_fails":     {condition: "If a check" + inNext + " finds CI failing on PR #{{.PR}}"},
	"next_if_review_comments": {condition: "{{if .Settle}}When the new review comments on PR #{{.PR}} settle" + inNext +
		"{{else}}If a check" + inNext + " finds new review comments on PR #{{.PR}}{{end}}"},
	"next_if_bot_comments": {condition: "If a check" + inNext + " finds " +
		"{{if .Spent}}a new review bot comment on the code of PR #{{.PR}}{{else}}new review bot comments on PR #{{.PR}}{{end}}"},
	"next_if_merge_conflict": {condition: "If a check" + inNext + " finds a new merge conflict on PR #{{.PR}}"},
	"next_when_merge_ready": {condition: "When a check" + inNext + " finds that PR #{{.PR}} " +
		"{{if .RequireCI}}passes CI and meets its other merge conditions{{else}}meets its merge conditions{{end}}"},
	"next_when_pr_merges": {condition: "When PR #{{.PR}} merges"},
	"next_if_label_added": {condition: "If `{{.Label}}` is added to PR #{{.PR}}"},

	"next_stage":      {text: "Sortie adds `{{.Label}}`, and a poll tick then starts stage {{.Next}} in a new session."},
	"next_stage_held": {text: "Sortie adds `{{.Label}}`, then holds {{.Key}} at its " + heldAt + " before stage {{.Next}} starts."},
	"next_handoff": {text: "{{if .AtCeiling}}The hop count is at its ceiling ({{count .Hops}} of {{count .Ceiling}}), so stage {{.Next}} does not start: {{end}}" +
		"Sortie moves {{.Key}} to {{.Handoff}}" +
		"{{if .NoChange}}, or to {{.NoChange}} if the agent reports that nothing needed changing{{end}}" +
		"{{if .Watch}}, and a pull request the run leaves gets the {{series .Watch}} {{if gt (len .Watch) 1}}reactions{{else}}reaction{{end}}{{end}}."},
	"next_continue":   {text: "Sortie dispatches another session for {{.Key}} if it is still in an active state."},
	"next_backoff":    {text: "Sortie retries {{.Key}} with exponential backoff."},
	"next_token_stop": {text: "Sortie stops the session and holds {{.Key}} at its token budget."},
	"next_dispatch": {text: "Sortie dispatches {{.Key}}" + onStage + " in a new session" +
		"{{if .ActiveState}} if it is still in an active state{{end}}."},
	"next_stage_start": {text: "Stage {{.Next}} starts in a new session."},
	"next_fix": {text: "Sortie dispatches {{.Run}} {{count .N}}" + ofMax +
		"{{if .Debounce}} once no newer comment arrives for {{duration .Debounce}}{{end}}."},
	"next_escalate": {text: "{{.Runs}} are used up ({{count .Used}} of {{count .Max}}), so Sortie " + posture + " and {{.After}}." +
		"{{if .Restart}} A new commit that no Sortie run made can restart the count.{{end}}"},
	"next_triage":           {text: "Sortie runs the triage command, and its answer decides what follows."},
	"next_held":             {text: "Sortie holds {{.Key}} at its " + heldAt + "."},
	"next_auto_merge":       {text: "Auto-merge merges it with the {{.Strategy}} strategy{{if .DeleteBranch}} and deletes its branch{{end}}."},
	"next_merge_completion": {text: "Sortie moves {{.Key}} to {{.State}}."},
	"next_label_command":    {text: "Sortie dispatches {{.What}} of the pull request" + afterAhead + "."},

	"ctl_refresh":           {text: "Tracker poll and reconciliation queued."},
	"ctl_refresh_coalesced": {text: "A tracker poll and reconciliation was already queued; this request joined it."},
	"ctl_refresh_refused":   {text: "Nothing queued: Sortie is shutting down."},
}

// templates is the parsed table. A source that does not parse panics at
// package initialization, so a broken table never reaches a request.
var templates = mustParse(table)

func mustParse(entries map[string]parts) *template.Template {
	root := template.New("sentence").Option("missingkey=error").Funcs(funcs)
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[name]
		for _, part := range []struct{ name, source string }{
			{"short", entry.short},
			{"long", entry.long},
			{"text", entry.text},
			{"condition", entry.condition},
		} {
			if part.source == "" {
				continue
			}
			if _, err := root.New(name + "." + part.name).Parse(part.source); err != nil {
				panic(fmt.Sprintf("sentence template %s: %v", name, err))
			}
		}
	}
	return root
}

// pausedReasons words the Error text of a paused retry.
var pausedReasons = map[string]string{
	"reaction retry paused by an open blocking issue": "an open issue blocks it",
	"reaction retry paused by issue state":            "the issue's tracker state does not allow it",
}

// rescheduleReasons words the Error text of a retry that came due and was
// not dispatched.
var rescheduleReasons = map[string]string{
	"worker still running":            "its previous run was still stopping",
	"retry issue fetch failed":        "re-reading the issue from the tracker failed",
	"issue without a state label":     "the issue carried no state label",
	"no available orchestrator slots": "no agent slot was free",
	"no available SSH hosts":          "no SSH host had a free slot",
	"retry agent kind unavailable":    "its agent kind is not available",
	"retry agent settings refused":    "its agent settings were refused",
}

// runPhrase names the run a reaction retry starts, sentence-initial and
// inline.
type runPhrase struct {
	initial string
	inline  string
}

// runPhrases is keyed by reaction kind.
var runPhrases = map[string]runPhrase{
	orchestrator.ReactionKindCI:            {"CI fix run", "CI fix run"},
	orchestrator.ReactionKindReview:        {"Review fix run", "review fix run"},
	orchestrator.ReactionKindBotReview:     {"Bot review fix run", "bot review fix run"},
	orchestrator.ReactionKindMergeConflict: {"Rebase run", "rebase run"},
	orchestrator.ReactionKindLabelReview:   {"Label review run", "label review run"},
	orchestrator.ReactionKindLabelFix:      {"Label fix run", "label fix run"},
}

// kindWords names a reaction kind sentence-initial and inline, and holds the
// label of the count of tries it has used; count is empty for a kind that
// counts none.
type kindWords struct {
	initial string
	inline  string
	count   string
}

// reactionKindWords is keyed by reaction kind.
var reactionKindWords = map[string]kindWords{
	orchestrator.ReactionKindCI:              {"CI", "CI", "CI fixes used"},
	orchestrator.ReactionKindReview:          {"Review comments", "review comments", "Review fix runs used"},
	orchestrator.ReactionKindBotReview:       {"Bot review", "bot review", "Bot review fix runs used"},
	orchestrator.ReactionKindAutoMerge:       {"Auto-merge", "auto-merge", "Merge calls made"},
	orchestrator.ReactionKindMergeConflict:   {"Merge conflict", "the merge conflict", "Rebase runs used"},
	orchestrator.ReactionKindMergeCompletion: {"Merge completion", "merge completion", "Failed tracker moves"},
	orchestrator.ReactionKindLabelReview:     {"Review label command", "the review label command", ""},
	orchestrator.ReactionKindLabelFix:        {"Fix label command", "the fix label command", ""},
}

// otherReactionKind words a kind the table does not know.
var otherReactionKind = kindWords{"Reaction", "the reaction", ""}

// waitClauses leads the next-check clause of a pending reaction, keyed by the
// entry's Wait value; the empty key is an entry no pass has taken yet.
var waitClauses = map[string]string{
	"":           "first check",
	"backoff":    "the last check did not complete; next check",
	"retry_slot": "another run or retry holds the issue; next check",
	"triage":     "it waits on its triage command; next check",
}

// otherWaitClause leads the next-check clause of any other Wait value.
const otherWaitClause = "next check"

// mergeability words what a merge-conflict reaction knows of the pull
// request, keyed by the state of its last check.
type mergeability string

const (
	mergeabilityUnchecked mergeability = "unchecked"
	mergeabilityUnknown   mergeability = "unknown"
	mergeabilityClear     mergeability = "clear"
)

var mergeabilityWords = map[mergeability]string{
	mergeabilityUnchecked: "no check has completed yet",
	mergeabilityUnknown:   "the provider has not computed mergeability yet",
	mergeabilityClear:     "the last check found none",
}

// mergePhrase words how a pull request merged around its number.
type mergePhrase struct {
	before string
	after  string
}

// mergePaths is keyed by the merge path an outcome serves.
var mergePaths = map[string]mergePhrase{
	"auto_merge":       {"auto-merge merged PR #", ""},
	"merge_completion": {"merge completion saw PR #", " merge"},
}

// otherMergePath words a merge path the table does not know.
var otherMergePath = mergePhrase{"PR #", " merged"}

// hopResults words why a stage hop was not made, keyed by the hop result.
var hopResults = map[string]string{
	"ceiling": "the issue reached its limit of consecutive stage moves (`dispatch.max_consecutive_hops`)",
	"failed":  "the next stage's label could not be added to the issue",
}

// fixWords words what a reaction kind runs when its watch finds its event, and
// what its pass does once the tries are used up.
type fixWords struct {
	run     string
	runs    string
	after   string
	restart bool // a head no Sortie run made resets the count
}

// fixVocabulary is keyed by the reaction kinds that count tries and run a fix.
var fixVocabulary = map[string]fixWords{
	orchestrator.ReactionKindCI:            {run: "CI fix", runs: "CI fixes", after: "keeps watching the pull request", restart: true},
	orchestrator.ReactionKindReview:        {run: "review fix", runs: "Review fixes", after: "stops watching for review comments"},
	orchestrator.ReactionKindBotReview:     {run: "bot review fix", runs: "Bot review fixes", after: "stops watching for bot review comments"},
	orchestrator.ReactionKindMergeConflict: {run: "rebase", runs: "Rebases", after: "stops watching for merge conflicts", restart: true},
}

// watchNames names, in the order a sentence lists them, what each reactions
// member watches for.
var watchNames = []struct {
	configKey string
	name      string
}{
	{"ci_failure", "CI failure"},
	{"review_comments", "review comments"},
	{"bot_review", "bot review"},
	{"merge_conflicts", "merge conflicts"},
	{"auto_merge", "auto-merge"},
	{"merge_completion", "merge completion"},
	{"label_commands", "label commands"},
}

// labelCommandWords words what a label command of each reaction kind
// dispatches.
var labelCommandWords = map[string]string{
	orchestrator.ReactionKindLabelReview: "a read-only review",
	orchestrator.ReactionKindLabelFix:    "a fix",
}
