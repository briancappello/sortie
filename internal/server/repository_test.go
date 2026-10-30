package server

import (
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// State shows each issue's repository.
func TestHandleState_Repository(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: started,
		Running: []orchestrator.SnapshotRunningEntry{
			{IssueID: "id-a", Identifier: "ENG-1", StartedAt: started, WorkspacePath: "/ws/ENG-1", Repository: "sortie"},
			{IssueID: "id-b", Identifier: "ENG-2", StartedAt: started, WorkspacePath: "/ws/ENG-2", Repository: "ao"},
			{IssueID: "id-c", Identifier: "ENG-3", StartedAt: started, WorkspacePath: "/ws/ENG-3"},
		},
	}
	ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
	body := getJSONMap(t, ts.URL+"/api/v1/state")
	got := map[string]any{}
	entries, _ := body["running"].([]any)
	for _, e := range entries {
		entry := e.(map[string]any)
		got[entry["issue_identifier"].(string)] = entry["repository"]
	}
	if got["ENG-1"] != "sortie" || got["ENG-2"] != "ao" || got["ENG-3"] != nil {
		t.Errorf("repositories = %v, want sortie, ao, and none", got)
	}
}
