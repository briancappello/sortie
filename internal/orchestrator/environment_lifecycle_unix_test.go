//go:build unix

package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/workspace"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

func TestRunWorkerAttempt_ReleasesTheEnvironmentLast(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cancel   bool // cancel the attempt from its first turn
		adapter  func(cancel context.CancelFunc) *mockAgentAdapter
		prepErr  error
		wantKind WorkerExitKind

		// wantAfterRun reports that after_run ran: every exit after a
		// successful preparation runs it.
		wantAfterRun bool
	}{
		{
			name:         "normal",
			adapter:      func(context.CancelFunc) *mockAgentAdapter { return &mockAgentAdapter{} },
			wantKind:     WorkerExitNormal,
			wantAfterRun: true,
		},
		{
			name: "failure",
			adapter: func(context.CancelFunc) *mockAgentAdapter {
				return &mockAgentAdapter{startSessionFn: func(context.Context, domain.StartSessionParams) (domain.Session, error) {
					return domain.Session{}, errors.New("agent unavailable")
				}}
			},
			wantKind:     WorkerExitError,
			wantAfterRun: true,
		},
		{
			name: "cancel",
			adapter: func(cancel context.CancelFunc) *mockAgentAdapter {
				return &mockAgentAdapter{runTurnFn: func(ctx context.Context, _ domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					cancel()
					<-ctx.Done()
					return domain.TurnResult{}, ctx.Err()
				}}
			},
			wantKind:     WorkerExitCancelled,
			wantAfterRun: true,
		},
		{
			name:     "preparation failure",
			adapter:  func(context.CancelFunc) *mockAgentAdapter { return &mockAgentAdapter{} },
			prepErr:  errors.New("vm start failed"),
			wantKind: WorkerExitError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			cfg := defaultWorkerConfig(root)
			cfg.Agent.MaxTurns = 1
			cfg.Hooks.AfterRun = "echo finished"
			env := recordingEnvironment(t)
			env.PrepareErr = tt.prepErr
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var opsAtExit []environmenttest.Operation
			ec := newExitCapture()
			RunWorkerAttempt(ctx, workerTestIssue(), nil, WorkerDeps{
				TrackerAdapter:         &mockTrackerAdapter{},
				AgentAdapter:           tt.adapter(cancel),
				ConfigFunc:             func() config.ServiceConfig { return cfg },
				PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
				OnEvent:                func(_ string, _ domain.AgentEvent) {},
				OnExit: func(issueID string, result WorkerResult) {
					opsAtExit = env.Ops()
					ec.onExit(issueID, result)
				},
				Logger: discardLogger(),
				EnvironmentFunc: func(config.EnvironmentConfig) (environment.Environment, error) {
					return env, nil
				},
			})
			result := ec.waitResult(t)

			if result.ExitKind != tt.wantKind {
				t.Errorf("ExitKind = %v (error %v), want %v", result.ExitKind, result.Error, tt.wantKind)
			}
			if len(opsAtExit) == 0 || opsAtExit[0] != environmenttest.OpPrepare || opsAtExit[len(opsAtExit)-1] != environmenttest.OpRelease ||
				slices.Index(opsAtExit, environmenttest.OpRelease) != len(opsAtExit)-1 {
				t.Errorf("environment ops at the exit report = %q, want Prepare first and one Release last: the release is the worker's last step", opsAtExit)
			}
			calls := env.Calls()
			if len(calls) != len(opsAtExit) {
				t.Errorf("environment ops after the exit report = %q, want none after %q", env.Ops(), opsAtExit)
			}
			if got := slices.Contains(shellScripts(env.Commands()), cfg.Hooks.AfterRun); got != tt.wantAfterRun {
				t.Errorf("after_run ran before the release = %v, want %v", got, tt.wantAfterRun)
			}
			release := calls[len(calls)-1]
			if want := filepath.Join(root, "TEST-1"); release.Op != environmenttest.OpRelease || release.Workspace.HostPath != want || release.Workspace.Key != "TEST-1" {
				t.Errorf("last call = %+v, want the release of workspace %s", release, want)
			}
		})
	}
}

// writeKindRecord records kind as the creator of the workspace at path.
func writeKindRecord(t *testing.T, path, kind string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, workspacekit.SortieDir), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := workspacekit.WriteSortieFile(path, workspace.EnvironmentRecordFile, []byte(`{"kind":"`+kind+`"}`)); err != nil {
		t.Fatalf("write environment record: %v", err)
	}
}

// recordedKindEnvironments returns the configured environment, the
// environment of the kind the workspace records, and a builder that
// hands out the latter and counts its calls.
func recordedKindEnvironments(t *testing.T) (configured, recorded *environmenttest.Recorder, forKind func(string) (environment.Environment, error), asked *[]string) {
	t.Helper()
	configured = recordingEnvironment(t)
	configured.KindName = config.EnvironmentKindLocal
	recorded = recordingEnvironment(t)
	recorded.KindName = staleKind
	var mu sync.Mutex
	kinds := []string{}
	forKind = func(kind string) (environment.Environment, error) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, kind)
		if kind != staleKind {
			return nil, errors.New("unexpected kind " + kind)
		}
		return recorded, nil
	}
	return configured, recorded, forKind, &kinds
}

func TestHandleWorkerExit_PendingCleanupUsesTheRecordedKind(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ENVKIND-1", nil)
	state.Running["ENVKIND-1"].PendingCleanup = true
	wsDir := filepath.Join(t.TempDir(), "ENVKIND-1-ident")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeKindRecord(t, wsDir, staleKind)
	configured, recorded, forKind, asked := recordedKindEnvironments(t)

	params := defaultExitParams(t, store)
	params.BeforeRemoveHook = "echo removing"
	params.HookTimeoutMS = 5000
	params.Environment = configured
	params.EnvironmentForKind = forKind
	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ENVKIND-1",
		Identifier:    "ENVKIND-1-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: wsDir,
	}, params)

	if !slices.Equal(*asked, []string{staleKind}) {
		t.Errorf("EnvironmentForKind asked for %q, want [%s]", *asked, staleKind)
	}
	if got := configured.Ops(); len(got) != 0 {
		t.Errorf("configured environment ops = %q, want none", got)
	}
	wantOps := []environmenttest.Operation{environmenttest.OpCommand, environmenttest.OpRemove}
	if got := recorded.Ops(); !slices.Equal(got, wantOps) {
		t.Errorf("recorded kind's ops = %q, want %q", got, wantOps)
	}
	if _, err := os.Stat(wsDir); !os.IsNotExist(err) {
		t.Errorf("workspace stat error = %v, want removed", err)
	}
}

func TestSweepWorkspaces_RemovesTerminalWorkspaceWithTheRecordedKind(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	wsPath := filepath.Join(root, "PROJ-KIND")
	mustMkdirSweep(t, wsPath)
	writeKindRecord(t, wsPath, staleKind)
	tracker := &sweepTracker{statesByKey: map[string]string{"PROJ-KIND": "Done"}}
	configured, recorded, forKind, _ := recordedKindEnvironments(t)
	params := defaultSweepParams(t, root, tracker)
	params.BeforeRemoveHook = "echo removing"
	params.HookTimeoutMS = 5000
	params.Environment = configured
	params.EnvironmentForKind = forKind

	SweepWorkspaces(NewState(5000, 4, 0, nil, AgentTotals{}), params)

	if got := configured.Ops(); len(got) != 0 {
		t.Errorf("configured environment ops = %q, want none", got)
	}
	wantOps := []environmenttest.Operation{environmenttest.OpCommand, environmenttest.OpRemove}
	if got := recorded.Ops(); !slices.Equal(got, wantOps) {
		t.Fatalf("recorded kind's ops = %q, want %q: before_remove in it, then its removal", got, wantOps)
	}
	assertSweepDirRemoved(t, wsPath)
}

func TestSweepEnvironments_PassesRootAndRunningKeys(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, 0, nil, AgentTotals{})
	state.Running["issue-a"] = &RunningEntry{Identifier: "RUN-1"}
	state.Running["issue-b"] = &RunningEntry{Identifier: "RUN 2"}
	root := t.TempDir()
	env := recordingEnvironment(t)
	var unselected []environment.SweepParams

	SweepEnvironments(SweepEnvironmentsParams{
		WorkspaceRoot: root,
		Environment:   env,
		SweepUnselected: func(_ context.Context, p environment.SweepParams) error {
			unselected = append(unselected, p)
			return errors.New("smolvm missing")
		},
		Running: RunningWorkspaceKeys(state, discardLogger()),
		Ctx:     context.Background(),
		Logger:  discardLogger(),
	})

	want := environment.SweepParams{WorkspaceRoot: root, Running: map[string]bool{"RUN-1": true, "RUN_2": true}}
	calls := env.Calls()
	if len(calls) != 1 || calls[0].Op != environmenttest.OpSweep {
		t.Fatalf("environment calls = %+v, want one sweep", calls)
	}
	assertSweepParams(t, "selected kind", calls[0].Sweep, want)
	if len(unselected) != 1 {
		t.Fatalf("unselected sweeps = %d, want 1", len(unselected))
	}
	assertSweepParams(t, "unselected kinds", unselected[0], want)
}

func assertSweepParams(t *testing.T, what string, got, want environment.SweepParams) {
	t.Helper()
	if got.WorkspaceRoot != want.WorkspaceRoot || len(got.Running) != len(want.Running) {
		t.Errorf("%s sweep params = %+v, want %+v", what, got, want)
		return
	}
	for key := range want.Running {
		if !got.Running[key] {
			t.Errorf("%s sweep params = %+v, want %+v", what, got, want)
			return
		}
	}
}

// sweepKind is an environment kind registered by this file's handleTick
// test: its constructor hands out the recorder registered for the
// workflow path, and its unselected sweep records the parameters of
// the sweep whose root a test registered.
const (
	sweepKind           = "orchestrator-test-sweep"
	unselectedSweepKind = "orchestrator-test-unselected-sweep"
)

var (
	sweepKindEnvs       sync.Map // workflow path -> *environmenttest.Recorder
	unselectedSweepSeen sync.Map // workspace root -> *[]environment.SweepParams
	unselectedSweepMu   sync.Mutex
	registerSweepKinds  sync.Once
)

func registerSweepTestKinds() {
	registerSweepKinds.Do(func() {
		environment.Registry.Register(sweepKind, func(p environment.Params) (environment.Environment, error) {
			env, ok := sweepKindEnvs.Load(p.WorkflowPath)
			if !ok {
				return nil, errors.New("no recorder for " + p.WorkflowPath)
			}
			return env.(*environmenttest.Recorder), nil
		})
		environment.Registry.RegisterWithMeta(unselectedSweepKind, func(environment.Params) (environment.Environment, error) {
			return &environmenttest.Recorder{KindName: unselectedSweepKind}, nil
		}, environment.Meta{SweepUnselected: func(_ context.Context, _ environment.Params, p environment.SweepParams) error {
			seen, ok := unselectedSweepSeen.Load(p.WorkspaceRoot)
			if !ok {
				return nil
			}
			unselectedSweepMu.Lock()
			defer unselectedSweepMu.Unlock()
			list := seen.(*[]environment.SweepParams)
			*list = append(*list, p)
			return nil
		}})
	})
}

// activeStatesTracker reports every issue it is asked about by ID as
// active, so reconciliation keeps a running entry running.
type activeStatesTracker struct {
	*sweepThrottleTracker
}

func (a activeStatesTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		out[id] = "To Do"
	}
	return out, nil
}

func TestHandleTick_SweepsEnvironmentsWithRunningKeys(t *testing.T) {
	t.Parallel()
	registerSweepTestKinds()

	root := t.TempDir()
	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	env := &environmenttest.Recorder{KindName: sweepKind}
	sweepKindEnvs.Store(workflowPath, env)
	var unselected []environment.SweepParams
	unselectedSweepSeen.Store(root, &unselected)

	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind: "mock", APIKey: "test-key",
			ActiveStates: []string{"To Do"}, TerminalStates: []string{"Done"},
		},
		Polling:     config.PollingConfig{IntervalMS: 60000},
		Workspace:   config.WorkspaceConfig{Root: root},
		Hooks:       config.HooksConfig{TimeoutMS: 5000},
		Agent:       config.AgentConfig{Kind: "mock", Command: "/usr/bin/agent", MaxConcurrentAgents: 1},
		Environment: config.EnvironmentConfig{Kind: sweepKind},
	}
	wm := &stubWorkflowManager{config: cfg, absPath: workflowPath}
	regs := passingPreflightRegistries()
	state := NewState(60000, 1, 0, nil, AgentTotals{})
	state.Running["issue-run"] = &RunningEntry{
		Identifier: "RUN-1",
		Issue:      domain.Issue{ID: "issue-run", Identifier: "RUN-1", State: "To Do"},
		StartedAt:  baseTime,
	}
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  activeStatesTracker{&sweepThrottleTracker{statesByKey: map[string]string{}}},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})

	ctx := context.Background()
	for range sweepEveryNTicks - 1 {
		o.handleTick(ctx)
	}
	if slices.Contains(env.Ops(), environmenttest.OpSweep) {
		t.Fatalf("environment ops = %q before tick %d, want no sweep yet", env.Ops(), sweepEveryNTicks)
	}
	o.handleTick(ctx)

	want := environment.SweepParams{WorkspaceRoot: root, Running: map[string]bool{"RUN-1": true}}
	var sweeps []environment.SweepParams
	for _, c := range env.Calls() {
		if c.Op == environmenttest.OpSweep {
			sweeps = append(sweeps, c.Sweep)
		}
	}
	if len(sweeps) != 1 {
		t.Fatalf("sweeps = %d, want 1 at tick %d", len(sweeps), sweepEveryNTicks)
	}
	assertSweepParams(t, "selected kind", sweeps[0], want)

	unselectedSweepMu.Lock()
	defer unselectedSweepMu.Unlock()
	if len(unselected) != 1 {
		t.Fatalf("unselected sweeps = %d, want 1: a kind the configuration no longer selects is swept too", len(unselected))
	}
	assertSweepParams(t, "unselected kind", unselected[0], want)
}

// TestRunWorkerAttempt_ReviewPosturePreparesTheEnvironment pins that a
// dispatch that runs no setup hook still prepares its workspace's
// execution environment, and finishes a newly created one, so its
// session never launches into an environment nothing started. The
// operator's hooks stay out of it.
func TestRunWorkerAttempt_ReviewPosturePreparesTheEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		existing        bool
		wantAfterCreate bool
	}{
		{name: "new workspace", wantAfterCreate: true},
		{name: "existing workspace", existing: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			cfg := defaultWorkerConfig(root)
			cfg.Agent.MaxTurns = 1
			cfg.Hooks.AfterCreate = "echo after-create"
			cfg.Hooks.BeforeRun = "echo before-run"
			cfg.Hooks.AfterRun = "echo after-run"
			wsPath := filepath.Join(root, "TEST-1")
			statusPath := filepath.Join(wsPath, workspacekit.SortieDir, "status")
			if tt.existing {
				if err := os.MkdirAll(filepath.Dir(statusPath), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(statusPath, []byte("done\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				// The workspace an earlier run prepared in this kind.
				writeKindRecord(t, wsPath, "recorder")
			}
			env := recordingEnvironment(t)

			var opsAtExit []environmenttest.Operation
			ec := newExitCapture()
			RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
				TrackerAdapter:         &mockTrackerAdapter{},
				AgentAdapter:           &mockAgentAdapter{},
				ConfigFunc:             func() config.ServiceConfig { return cfg },
				PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
				OnEvent:                func(_ string, _ domain.AgentEvent) {},
				OnExit: func(issueID string, result WorkerResult) {
					opsAtExit = env.Ops()
					ec.onExit(issueID, result)
				},
				Logger:  discardLogger(),
				Posture: PostureReview,
				EnvironmentFunc: func(config.EnvironmentConfig) (environment.Environment, error) {
					return env, nil
				},
			})
			result := ec.waitResult(t)

			if result.ExitKind != WorkerExitNormal || result.WorkspacePath != wsPath {
				t.Fatalf("result = kind %v, workspace %q, error %v; want a normal exit in %s", result.ExitKind, result.WorkspacePath, result.Error, wsPath)
			}
			if len(opsAtExit) == 0 || opsAtExit[0] != environmenttest.OpPrepare {
				t.Errorf("environment ops = %q, want Prepare first: the session needs a prepared environment", opsAtExit)
			}
			if got := slices.Contains(opsAtExit, environmenttest.OpAfterCreate); got != tt.wantAfterCreate {
				t.Errorf("environment ops = %q, AfterCreate called = %v, want %v", opsAtExit, got, tt.wantAfterCreate)
			}
			if got := shellScripts(env.Commands()); len(got) != 0 {
				t.Errorf("hooks run = %q, want none: the posture runs no operator hook", got)
			}
			if opsAtExit[len(opsAtExit)-1] != environmenttest.OpRelease {
				t.Errorf("environment ops = %q, want the release last", opsAtExit)
			}
			if _, err := os.Stat(statusPath); !os.IsNotExist(err) {
				t.Errorf("stale status Stat error = %v, want it cleared before the session", err)
			}
		})
	}
}
