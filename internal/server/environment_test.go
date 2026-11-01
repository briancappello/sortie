package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// environmentSnapshot holds a running and a retrying issue whose
// workspaces run in VMs, and one of each that run on the host.
func environmentSnapshot() orchestrator.RuntimeSnapshotResult {
	started := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	return orchestrator.RuntimeSnapshotResult{
		GeneratedAt: started.Add(time.Minute),
		Running: []orchestrator.SnapshotRunningEntry{
			{IssueID: "id-vm", Identifier: "ENG-42", StartedAt: started, WorkspacePath: "/srv/ws/ENG-42",
				Environment: &orchestrator.SnapshotEnvironment{Kind: "smolvm", Name: "sortie-ENG-42"}},
			{IssueID: "id-host", Identifier: "ENG-1", StartedAt: started, WorkspacePath: "/srv/ws/ENG-1"},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{IssueID: "id-vm-retry", Identifier: "ENG-43", Attempt: 2, DueAtMS: started.UnixMilli(),
				Environment: &orchestrator.SnapshotEnvironment{Kind: "smolvm", Name: "sortie-ENG-43"}},
			{IssueID: "id-host-retry", Identifier: "ENG-2", Attempt: 2, DueAtMS: started.UnixMilli()},
		},
	}
}

func getJSONMap(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // the test server's own URL
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return body
}

// wantEnvironment fails unless entry holds the environment object
// {kind, name}, or, with an empty kind, no environment key at all.
func wantEnvironment(t *testing.T, where string, entry map[string]any, kind, name string) {
	t.Helper()
	got, present := entry["environment"]
	if kind == "" {
		if present {
			t.Errorf("%s environment = %v, want the key omitted", where, got)
		}
		return
	}
	env, ok := got.(map[string]any)
	if !ok || len(env) != 2 || env["kind"] != kind || env["name"] != name {
		t.Errorf("%s environment = %v, want {kind: %s, name: %s}", where, got, kind, name)
	}
}

func TestHandleState_Environment(t *testing.T) {
	t.Parallel()

	ts := testServer(t, fixedSnapshot(environmentSnapshot()), acceptingRefresh())
	body := getJSONMap(t, ts.URL+"/api/v1/state")

	byIdentifier := map[string]map[string]any{}
	for _, list := range []string{"running", "retrying"} {
		entries, _ := body[list].([]any)
		for _, e := range entries {
			entry := e.(map[string]any)
			byIdentifier[entry["issue_identifier"].(string)] = entry
		}
	}
	wantEnvironment(t, "running ENG-42", byIdentifier["ENG-42"], "smolvm", "sortie-ENG-42")
	wantEnvironment(t, "running ENG-1", byIdentifier["ENG-1"], "", "")
	wantEnvironment(t, "retrying ENG-43", byIdentifier["ENG-43"], "smolvm", "sortie-ENG-43")
	wantEnvironment(t, "retrying ENG-2", byIdentifier["ENG-2"], "", "")
}

func TestHandleIssueDetail_Environment(t *testing.T) {
	t.Parallel()

	ts := testServer(t, fixedSnapshot(environmentSnapshot()), acceptingRefresh())
	tests := []struct {
		identifier string
		section    string
		kind, name string
	}{
		{identifier: "ENG-42", section: "running", kind: "smolvm", name: "sortie-ENG-42"},
		{identifier: "ENG-1", section: "running"},
		{identifier: "ENG-43", section: "retry", kind: "smolvm", name: "sortie-ENG-43"},
		{identifier: "ENG-2", section: "retry"},
	}
	for _, tt := range tests {
		body := getJSONMap(t, ts.URL+"/api/v1/"+tt.identifier)
		wantEnvironment(t, tt.identifier+" detail", body, tt.kind, tt.name)
		section, ok := body[tt.section].(map[string]any)
		if !ok {
			t.Fatalf("%s detail %s = %v, want an object", tt.identifier, tt.section, body[tt.section])
		}
		wantEnvironment(t, tt.identifier+" detail "+tt.section, section, tt.kind, tt.name)
	}
}

// GET /api/v1/state names the host path of each kept directory of a
// running, retrying, or held issue's workspace.
func TestHandleState_KeptDirs(t *testing.T) {
	t.Parallel()

	kept := func(key string) *orchestrator.SnapshotEnvironment {
		return &orchestrator.SnapshotEnvironment{Kind: "smolvm", Name: "sortie-" + key, KeptDirs: []string{"/srv/recorder/" + key}}
	}
	snap := environmentSnapshot()
	snap.Running[0].Environment = kept("ENG-42")
	snap.Retrying[0].Environment = kept("ENG-43")
	snap.Held = []orchestrator.SnapshotHeldEntry{
		{IssueID: "id-held", Identifier: "ENG-44", Hold: orchestrator.SnapshotHold{Reason: orchestrator.SkipSSHHostsFull}, Environment: kept("ENG-44")},
		{IssueID: "id-held-host", Identifier: "ENG-3", Hold: orchestrator.SnapshotHold{Reason: orchestrator.SkipSSHHostsFull}},
	}
	ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
	body := getJSONMap(t, ts.URL+"/api/v1/state")

	byIdentifier := map[string]map[string]any{}
	for _, list := range []string{"running", "retrying", "held"} {
		entries, _ := body[list].([]any)
		for _, e := range entries {
			entry := e.(map[string]any)
			byIdentifier[entry["issue_identifier"].(string)] = entry
		}
	}
	for _, key := range []string{"ENG-42", "ENG-43", "ENG-44"} {
		env, _ := byIdentifier[key]["environment"].(map[string]any)
		dirs, _ := env["kept_dirs"].([]any)
		if len(dirs) != 1 || dirs[0] != "/srv/recorder/"+key {
			t.Errorf("%s environment = %v, want kept_dirs [/srv/recorder/%s]", key, env, key)
		}
	}
	for _, key := range []string{"ENG-1", "ENG-2", "ENG-3"} {
		if env, present := byIdentifier[key]["environment"]; present {
			t.Errorf("%s environment = %v, want the key omitted", key, env)
		}
	}
}
