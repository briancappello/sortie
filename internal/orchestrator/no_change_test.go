package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

type fakeMirror struct {
	onBase map[string]bool
	err    error
}

func (m fakeMirror) CommitOnBase(_ context.Context, sha string) (bool, error) {
	return m.onBase[sha], m.err
}

type planKeyTracker struct {
	domain.TrackerAdapter
	ids map[string]string
}

func (p planKeyTracker) FindIssueByPlanKey(_ context.Context, key string) (domain.PlanIssue, error) {
	if id, ok := p.ids[key]; ok {
		return domain.PlanIssue{ID: id, PlanKey: key}, nil
	}
	return domain.PlanIssue{}, &domain.TrackerError{Kind: domain.ErrTrackerNotFound}
}

func (p planKeyTracker) ListPlanIssues(context.Context, bool) ([]domain.PlanIssue, error) {
	return nil, nil
}

func evidenceWorkspace(t *testing.T, doc string) string {
	t.Helper()
	ws := t.TempDir()
	if doc == "" {
		return ws
	}
	if err := os.MkdirAll(filepath.Join(ws, ".sortie"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".sortie", "evidence.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestConfirmNoChange(t *testing.T) {
	t.Parallel()
	onBase, offBase := strings.Repeat("a", 40), strings.Repeat("b", 40)
	mirror := fakeMirror{onBase: map[string]bool{onBase: true}}
	tracker := planKeyTracker{ids: map[string]string{"payments/http-client": "iss-http", "billing/retry": "iss-self"}}
	tests := []struct {
		name       string
		doc        string
		repo       any
		wantRef    string
		wantCites  string
		wantReason string
	}{
		{"a commit on base", `{"kind":"commit","sha":"` + onBase + `"}`, mirror, "commit:" + onBase, "", ""},
		{"a commit off base", `{"kind":"commit","sha":"` + offBase + `"}`, mirror, "", "", NoChangeCommitNotOnBase},
		{"no mirror", `{"kind":"commit","sha":"` + onBase + `"}`, nil, "", "", NoChangeNoMirror},
		{"a broken mirror", `{"kind":"commit","sha":"` + onBase + `"}`, fakeMirror{err: errors.New("locked")}, "", "", NoChangeCheckFailed},
		{"a plan story", `{"kind":"plan_key","plan_key":"payments/http-client"}`, nil, "plan:payments/http-client", "iss-http", ""},
		{"an unknown plan key", `{"kind":"plan_key","plan_key":"payments/nope"}`, nil, "", "", NoChangeUnknownPlanKey},
		{"citing itself", `{"kind":"plan_key","plan_key":"billing/retry"}`, nil, "", "", NoChangeCycle},
		{"no evidence", "", mirror, "", "", "no_evidence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := confirmNoChange(context.Background(), tracker, tt.repo, evidenceWorkspace(t, tt.doc), "iss-self", discardLogger())
			if !v.Checked || v.Ref != tt.wantRef || v.Cites != tt.wantCites || v.Unverified != tt.wantReason {
				t.Errorf("verdict = %+v, want ref %q cites %q unverified %q", v, tt.wantRef, tt.wantCites, tt.wantReason)
			}
		})
	}
}

type satisfactionStore struct {
	mu   sync.Mutex
	rows map[string][]persistence.PlanSatisfaction
}

func (s *satisfactionStore) UpsertPlanSatisfaction(_ context.Context, r persistence.PlanSatisfaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string][]persistence.PlanSatisfaction{}
	}
	s.rows[r.IssueID] = append(s.rows[r.IssueID], r)
	return nil
}

func (s *satisfactionStore) LoadPlanSatisfaction(_ context.Context, ids []string) (map[string][]persistence.PlanSatisfaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]persistence.PlanSatisfaction{}
	for _, id := range ids {
		if r, ok := s.rows[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

func TestRecordNoChange(t *testing.T) {
	t.Parallel()
	store := &satisfactionStore{}
	ctx := context.Background()
	recordNoChange(ctx, store, "a", NoChangeVerdict{Checked: true, Ref: "commit:x"}, discardLogger())
	recordNoChange(ctx, store, "b", NoChangeVerdict{Checked: true, Unverified: NoChangeNoMirror}, discardLogger())
	recordNoChange(ctx, store, "c", NoChangeVerdict{}, discardLogger())
	if r := store.rows["a"]; len(r) != 1 || r[0].Kind != persistence.PlanSatisfactionNoChange {
		t.Errorf("a = %+v, want a confirmed claim", r)
	}
	if r := store.rows["b"]; len(r) != 1 || r[0].Kind != persistence.PlanSatisfactionUnverified || r[0].Ref != NoChangeNoMirror {
		t.Errorf("b = %+v, want an unverified claim", r)
	}
	if _, ok := store.rows["c"]; ok {
		t.Error("a run with no claim recorded one")
	}

	// Two stories that cite each other: the second is a cycle.
	recordNoChange(ctx, store, "x", NoChangeVerdict{Checked: true, Ref: "plan:y", Cites: "y"}, discardLogger())
	recordNoChange(ctx, store, "y", NoChangeVerdict{Checked: true, Ref: "plan:x", Cites: "x"}, discardLogger())
	if r := store.rows["y"]; len(r) != 1 || r[0].Kind != persistence.PlanSatisfactionUnverified || r[0].Ref != NoChangeCycle {
		t.Errorf("y = %+v, want an unverified cycle", r)
	}

	// The judge: a plan claim counts once the cited story merged.
	_ = store.UpsertPlanSatisfaction(ctx, persistence.PlanSatisfaction{IssueID: "http", Kind: persistence.PlanSatisfactionMerged, Ref: "m"})
	recordNoChange(ctx, store, "retry", NoChangeVerdict{Checked: true, Ref: "plan:payments/http-client", Cites: "http"}, discardLogger())
	j := NewPlanJudge(ctx, store, "Canceled", "Done", discardLogger())
	if why := j.Unsatisfied(domain.BlockerRef{ID: "retry", PlanKey: "billing/retry", State: "Done"}, planTerminal); why != "" {
		t.Errorf("retry, citing a merged story = %q, want satisfied", why)
	}
}
