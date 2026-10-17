package persistence

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Plan satisfaction kinds.
const (
	PlanSatisfactionMerged     = "merged"
	PlanSatisfactionNoChange   = "no_change"
	PlanSatisfactionUnverified = "unverified_no_change"
)

// PlanSatisfaction is a durable record that an issue's work is done in a
// way that satisfies the plan stories it blocks.
type PlanSatisfaction struct {
	IssueID    string
	Kind       string // one of the PlanSatisfaction kinds
	Ref        string // merge commit; commit:<sha> or plan:<key>; or the unverified reason
	PR         string // owner/repo#number for a merge; empty otherwise
	Cites      string // issue id of the story a plan-key claim cites
	RecordedAt time.Time
}

// UpsertPlanSatisfaction records r, replacing the row of the same issue
// and kind. Rows are never deleted.
func (s *Store) UpsertPlanSatisfaction(ctx context.Context, r PlanSatisfaction) error {
	at := r.RecordedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plan_satisfaction (issue_id, kind, ref, pr, cites, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id, kind) DO UPDATE SET
			ref = excluded.ref, pr = excluded.pr, cites = excluded.cites, recorded_at = excluded.recorded_at`,
		r.IssueID, r.Kind, r.Ref, r.PR, r.Cites, at.UTC().Format(time.RFC3339),
	)
	return err
}

// planSatisfactionBatch bounds the number of ids in one query, below
// SQLite's default host parameter limit.
const planSatisfactionBatch = 900

// LoadPlanSatisfaction returns the rows for the given issue ids, keyed by
// issue id. Ids without a row are absent from the result.
func (s *Store) LoadPlanSatisfaction(ctx context.Context, issueIDs []string) (map[string][]PlanSatisfaction, error) {
	out := make(map[string][]PlanSatisfaction)
	for start := 0; start < len(issueIDs); start += planSatisfactionBatch {
		batch := issueIDs[start:min(start+planSatisfactionBatch, len(issueIDs))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT issue_id, kind, ref, pr, cites, recorded_at FROM plan_satisfaction WHERE issue_id IN (`+
				strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("query plan_satisfaction: %w", err)
		}
		for rows.Next() {
			var r PlanSatisfaction
			var at string
			if err := rows.Scan(&r.IssueID, &r.Kind, &r.Ref, &r.PR, &r.Cites, &at); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan plan_satisfaction: %w", err)
			}
			r.RecordedAt, _ = time.Parse(time.RFC3339, at)
			out[r.IssueID] = append(out[r.IssueID], r)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("iterate plan_satisfaction: %w", err)
		}
	}
	return out, nil
}
