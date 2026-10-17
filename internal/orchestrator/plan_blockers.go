package orchestrator

import (
	"context"
	"log/slog"
	"maps"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// Why a blocker does not let the stories it blocks start.
const (
	BlockerNotMerged          = "not_merged"
	BlockerAwaitingMerge      = "awaiting_merge_record"
	BlockerCanceled           = "canceled"
	BlockerNotDone            = "not_done"
	BlockerUnverifiedNoChange = "unverified_no_change"
	BlockerUnknown            = "unknown"
	BlockerNotTerminal        = "not_terminal"
)

// BlockerJudge decides whether one blocker of a candidate lets it start.
// It runs on the event loop.
type BlockerJudge interface {
	// Unsatisfied returns the reason the blocker does not satisfy its
	// dependent, or "" when it does.
	Unsatisfied(b domain.BlockerRef, terminalSet map[string]struct{}) string
}

// terminalJudge is the rule without plan control: a blocker in a terminal
// state is satisfied.
type terminalJudge struct{}

func (terminalJudge) Unsatisfied(b domain.BlockerRef, terminalSet map[string]struct{}) string {
	if b.State == "" {
		return BlockerUnknown
	}
	if _, terminal := terminalSet[strings.ToLower(b.State)]; !terminal {
		return BlockerNotTerminal
	}
	return ""
}

// PlanJudge applies the plan-control rule. A blocker with a plan key is
// satisfied only by Sortie's own durable record of a merge or of a
// confirmed no-change claim. A blocker without one is satisfied only by
// the done state. A cancelled blocker never is.
type PlanJudge struct {
	CancelState string
	DoneState   string

	loader PlanSatisfactionLoader
	ctx    context.Context
	log    *slog.Logger
	rows   map[string][]persistence.PlanSatisfaction
	loaded map[string]bool
	failed bool
}

// NewPlanJudge returns a judge that reads satisfaction through loader,
// caching what it reads. Prime it with the ids a tick will ask about.
func NewPlanJudge(ctx context.Context, loader PlanSatisfactionLoader, cancelState, doneState string, log *slog.Logger) *PlanJudge {
	return &PlanJudge{
		CancelState: cancelState, DoneState: doneState,
		loader: loader, ctx: ctx, log: log,
		rows: map[string][]persistence.PlanSatisfaction{}, loaded: map[string]bool{},
	}
}

// Prime loads the rows of ids in one query, and the rows of the stories
// those rows cite in a second.
func (j *PlanJudge) Prime(ids []string) {
	j.load(ids)
	var cited []string
	for _, id := range ids {
		for _, r := range j.rows[id] {
			if r.Cites != "" {
				cited = append(cited, r.Cites)
			}
		}
	}
	j.load(cited)
}

func (j *PlanJudge) load(ids []string) {
	var need []string
	for _, id := range ids {
		if id != "" && !j.loaded[id] {
			need = append(need, id)
			j.loaded[id] = true
		}
	}
	if len(need) == 0 || j.loader == nil {
		return
	}
	got, err := j.loader.LoadPlanSatisfaction(j.ctx, need)
	if err != nil {
		// Without the records nothing is satisfied: hold, never guess.
		j.failed = true
		if j.log != nil {
			j.log.Warn("plan satisfaction read failed, holding plan stories", slog.Any("error", err))
		}
		return
	}
	maps.Copy(j.rows, got)
}

func (j *PlanJudge) row(id, kind string) (persistence.PlanSatisfaction, bool) {
	j.load([]string{id})
	for _, r := range j.rows[id] {
		if r.Kind == kind {
			return r, true
		}
	}
	return persistence.PlanSatisfaction{}, false
}

// satisfied reports whether the issue's own records satisfy dependents.
// A plan-key claim counts while the cited story is satisfied, one level
// deep.
func (j *PlanJudge) satisfied(id string, depth int) bool {
	if j.failed {
		return false
	}
	if _, ok := j.row(id, persistence.PlanSatisfactionMerged); ok {
		return true
	}
	r, ok := j.row(id, persistence.PlanSatisfactionNoChange)
	if !ok {
		return false
	}
	if r.Cites == "" {
		return true
	}
	if depth > 0 || r.Cites == id {
		return false
	}
	return j.satisfied(r.Cites, depth+1)
}

// Satisfaction reports what the issue's records say: merged,
// no_change_confirmed, unverified_no_change, or "".
func (j *PlanJudge) Satisfaction(id string) string {
	switch {
	case j.has(id, persistence.PlanSatisfactionMerged):
		return "merged"
	case j.has(id, persistence.PlanSatisfactionNoChange):
		return "no_change_confirmed"
	case j.has(id, persistence.PlanSatisfactionUnverified):
		return "unverified_no_change"
	}
	return ""
}

func (j *PlanJudge) has(id, kind string) bool {
	_, ok := j.row(id, kind)
	return ok
}

// Record returns the issue's row of the given kind.
func (j *PlanJudge) Record(id, kind string) (persistence.PlanSatisfaction, bool) {
	return j.row(id, kind)
}

// Unsatisfied implements [BlockerJudge].
func (j *PlanJudge) Unsatisfied(b domain.BlockerRef, terminalSet map[string]struct{}) string {
	if b.State == "" {
		return BlockerUnknown
	}
	if strings.EqualFold(b.State, j.CancelState) {
		return BlockerCanceled
	}
	if b.PlanKey == "" {
		if strings.EqualFold(b.State, j.DoneState) {
			return ""
		}
		return BlockerNotDone
	}
	if b.ID != "" && j.satisfied(b.ID, 0) {
		return ""
	}
	if b.ID != "" && j.has(b.ID, persistence.PlanSatisfactionUnverified) {
		return BlockerUnverifiedNoChange
	}
	if _, terminal := terminalSet[strings.ToLower(b.State)]; terminal {
		return BlockerAwaitingMerge
	}
	return BlockerNotMerged
}

// PlanJudgeFor returns a judge primed with the blockers of candidates when
// plan control is on, and nil otherwise. A store that cannot load
// satisfaction yields a judge that satisfies no plan-keyed blocker.
func PlanJudgeFor(ctx context.Context, tracker config.TrackerConfig, store any, candidates []domain.Issue, log *slog.Logger) BlockerJudge {
	if !tracker.PlanControl() {
		return nil
	}
	loader, _ := store.(PlanSatisfactionLoader)
	j := NewPlanJudge(ctx, loader, tracker.CancelState, tracker.DoneState, log)
	j.Prime(BlockerIDs(candidates))
	return j
}

// judgeOf returns the state's judge, or the terminal-state rule.
func judgeOf(state *State) BlockerJudge {
	if state != nil && state.BlockerJudge != nil {
		return state.BlockerJudge
	}
	return terminalJudge{}
}

// firstUnsatisfiedBlocker returns the first blocker the judge does not
// accept, with the reason, or a zero blocker and "".
func firstUnsatisfiedBlocker(judge BlockerJudge, blockers []domain.BlockerRef, terminalSet map[string]struct{}) (domain.BlockerRef, string) {
	for _, b := range blockers {
		if reason := judge.Unsatisfied(b, terminalSet); reason != "" {
			return b, reason
		}
	}
	return domain.BlockerRef{}, ""
}

// isBlockedFor is the one blocker-hold decision. An issue whose
// BlockersUnresolved is true is blocked, because BlockedBy is not
// authoritative for it.
func isBlockedFor(judge BlockerJudge, issue domain.Issue, terminalSet map[string]struct{}) bool {
	if issue.BlockersUnresolved {
		return true
	}
	_, reason := firstUnsatisfiedBlocker(judge, issue.BlockedBy, terminalSet)
	return reason != ""
}

// BlockerIDs returns the ids of every blocker of issues.
func BlockerIDs(issues []domain.Issue) []string {
	var ids []string
	for _, is := range issues {
		for _, b := range is.BlockedBy {
			if b.ID != "" {
				ids = append(ids, b.ID)
			}
		}
	}
	return ids
}
