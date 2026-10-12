//go:build unix

package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// recordingEnvironment returns a recorder that runs every command through
// the registered local kind, so the workspace code still runs while the
// test sees that it went through the environment.
func recordingEnvironment(t *testing.T) *environmenttest.Recorder {
	t.Helper()
	inner, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: config.EnvironmentKindLocal}})
	if err != nil {
		t.Fatalf("environment.New(local) error = %v", err)
	}
	return &environmenttest.Recorder{Inner: inner}
}

// shellScripts returns the scripts of the recorded "sh -c" commands, in
// order.
func shellScripts(cmds []environmenttest.Call) []string {
	var scripts []string
	for _, c := range cmds {
		if len(c.Spec.Argv) == 3 && c.Spec.Argv[0] == "sh" && c.Spec.Argv[1] == "-c" {
			scripts = append(scripts, c.Spec.Argv[2])
		}
	}
	return scripts
}

// gitSubcommands returns the subcommand of each recorded git command,
// the first argument after "git" and its global options, in order.
func gitSubcommands(cmds []environmenttest.Call) []string {
	var subs []string
	for _, c := range cmds {
		if len(c.Spec.Argv) < 2 || c.Spec.Argv[0] != "git" {
			continue
		}
		args := c.Spec.Argv[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			if args[0] == "-c" || args[0] == "-C" {
				args = args[1:]
			}
			args = args[1:]
		}
		if len(args) > 0 {
			subs = append(subs, args[0])
		}
	}
	return subs
}

func TestRunWorkerAttempt_WorkspaceCodeRunsThroughEnvironment(t *testing.T) {
	root := t.TempDir()
	cfg := defaultWorkerConfig(root)
	cfg.Agent.MaxTurns = 1
	cfg.Tracker.HandoffEvidence = config.HandoffEvidenceObserved
	cfg.Environment = config.EnvironmentConfig{Kind: config.EnvironmentKindLocal, HostPrefix: []string{"marker"}}
	afterCreate := "git init -q && git config commit.gpgsign false && git config user.email test@example.com && git config user.name Test && git commit -q --allow-empty -m initial"
	beforeRun := "echo prepared > before-run.txt"
	afterRun := "echo finished > after-run.txt"
	cfg.Hooks.AfterCreate = afterCreate
	cfg.Hooks.BeforeRun = beforeRun
	cfg.Hooks.AfterRun = afterRun

	env := recordingEnvironment(t)
	var resolved []config.EnvironmentConfig
	ec := newExitCapture()
	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		EnvironmentFunc: func(envCfg config.EnvironmentConfig) (environment.Environment, error) {
			resolved = append(resolved, envCfg)
			return env, nil
		},
	})
	result := ec.waitResult(t)

	if len(resolved) != 1 || !slices.Equal(resolved[0].HostPrefix, []string{"marker"}) {
		t.Errorf("EnvironmentFunc calls = %+v, want one with the attempt-start environment block", resolved)
	}
	if ops := env.Ops(); len(ops) == 0 || ops[0] != environmenttest.OpPrepare {
		t.Fatalf("environment ops = %q, want Prepare first", ops)
	}
	cmds := env.Commands()
	if got, want := shellScripts(cmds), []string{afterCreate, beforeRun, afterRun}; !slices.Equal(got, want) {
		t.Errorf("hook scripts run through the environment = %q, want %q", got, want)
	}
	if subs := gitSubcommands(cmds); !slices.Contains(subs, "rev-parse") || !slices.Contains(subs, "status") {
		t.Errorf("git commands run through the environment = %q, want the handoff baseline inspection", subs)
	}
	for _, c := range cmds {
		if c.Workspace.HostPath != result.WorkspacePath {
			t.Errorf("command %q workspace = %q, want %q", c.Spec.Argv, c.Workspace.HostPath, result.WorkspacePath)
		}
	}
	if result.HandoffEvidenceBaseline == nil {
		t.Errorf("HandoffEvidenceBaseline = nil (error %v), want one captured through the environment", result.HandoffEvidenceBaselineError)
	}
	for _, name := range []string{"before-run.txt", "after-run.txt"} {
		if _, err := os.Stat(filepath.Join(result.WorkspacePath, name)); err != nil {
			t.Errorf("hook output %s: %v", name, err)
		}
	}
}

func TestRunWorkerAttempt_EnvironmentUnavailableFailsPreparation(t *testing.T) {
	root := t.TempDir()
	cfg := defaultWorkerConfig(root)
	marker := filepath.Join(t.TempDir(), "after-create-ran")
	cfg.Hooks.AfterCreate = "touch " + marker
	resolveErr := errors.New("no such kind")

	ec := newExitCapture()
	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		EnvironmentFunc: func(config.EnvironmentConfig) (environment.Environment, error) {
			return nil, resolveErr
		},
	})
	result := ec.waitResult(t)

	if !errors.Is(result.Error, resolveErr) || !strings.Contains(result.Error.Error(), "workspace preparation") {
		t.Errorf("result error = %v, want a workspace preparation error wrapping %v", result.Error, resolveErr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("after_create marker stat error = %v, want the hook never run on the host", err)
	}
}

func TestHandleWorkerExit_PendingCleanupRunsThroughEnvironment(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ENVCLEAN-1", nil)
	state.Running["ENVCLEAN-1"].PendingCleanup = true
	state.Running["ENVCLEAN-1"].Identifier = "ENVCLEAN-1-ident"
	wsDir := filepath.Join(t.TempDir(), "ENVCLEAN-1-ident")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	env := recordingEnvironment(t)
	params := defaultExitParams(t, store)
	params.BeforeRemoveHook = "echo removing"
	params.HookTimeoutMS = 5000
	params.Environment = env

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ENVCLEAN-1",
		Identifier:    "ENVCLEAN-1-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: wsDir,
	}, params)

	wantOps := []environmenttest.Operation{environmenttest.OpCommand, environmenttest.OpRemove}
	if got := env.Ops(); !slices.Equal(got, wantOps) {
		t.Fatalf("environment ops = %q, want %q", got, wantOps)
	}
	if got := shellScripts(env.Commands()); !slices.Equal(got, []string{"echo removing"}) {
		t.Errorf("before_remove scripts = %q, want [echo removing]", got)
	}
	if _, err := os.Stat(wsDir); !os.IsNotExist(err) {
		t.Errorf("workspace stat error = %v, want removed", err)
	}
}

func TestHandleWorkerExit_HandoffEvidenceComparesThroughEnvironment(t *testing.T) {
	const issueID = "ENVHE-1"
	dir, baseline := handoffEvidenceGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, issueID, "In Progress")
	env := recordingEnvironment(t)
	params := handoffEvidenceExitParams(t, store, tracker, &domain.NoopMetrics{})
	params.Environment = env

	HandleWorkerExit(state, WorkerResult{
		IssueID:                 issueID,
		Identifier:              issueID + "-ident",
		ExitKind:                WorkerExitNormal,
		TurnsCompleted:          1,
		WorkspacePath:           dir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: baseline,
		AgentAdapter:            "mock",
	}, params)
	t.Cleanup(func() { CancelRetry(state, issueID) })

	cmds := env.Commands()
	if subs := gitSubcommands(cmds); !slices.Contains(subs, "rev-parse") || !slices.Contains(subs, "ls-files") {
		t.Errorf("git commands run through the environment = %q, want the evidence comparison", subs)
	}
	for _, c := range cmds {
		if c.Workspace.HostPath != dir || c.Workspace.IssueID != issueID {
			t.Errorf("command workspace = %+v, want host path %q and issue %q", c.Workspace, dir, issueID)
		}
	}
	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue calls = %d, want 1: the untracked file counts as work observed", len(tracker.transitionCalls))
	}
}

func TestSweepWorkspaces_RemovesThroughEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, params *SweepWorkspacesParams, tracker *sweepTracker, wsPath string)
	}{
		{
			name: "terminal",
			setup: func(_ *testing.T, _ *SweepWorkspacesParams, tracker *sweepTracker, _ string) {
				tracker.statesByKey["PROJ-ENV"] = "Done"
			},
		},
		{
			name: "age",
			setup: func(t *testing.T, params *SweepWorkspacesParams, _ *sweepTracker, wsPath string) {
				writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())
				params.RetentionDays = config.WorkspaceRetentionMinDays
				params.Store = &sweepStoreDouble{}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			wsPath := filepath.Join(root, "PROJ-ENV")
			mustMkdirSweep(t, wsPath)
			tracker := &sweepTracker{statesByKey: map[string]string{}}
			env := recordingEnvironment(t)
			params := defaultSweepParams(t, root, tracker)
			params.BeforeRemoveHook = "echo removing"
			params.HookTimeoutMS = 5000
			params.Environment = env
			tt.setup(t, &params, tracker, wsPath)

			SweepWorkspaces(NewState(5000, 4, 0, nil, AgentTotals{}), params)

			wantOps := []environmenttest.Operation{environmenttest.OpCommand, environmenttest.OpRemove}
			if got := env.Ops(); !slices.Equal(got, wantOps) {
				t.Fatalf("environment ops = %q, want %q", got, wantOps)
			}
			assertSweepDirRemoved(t, wsPath)
		})
	}
}

func TestSelfReviewLoop_RunsDiffAndVerificationThroughEnvironment(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	initGitRepo(t, wsPath)
	if err := os.WriteFile(filepath.Join(wsPath, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg := selfReviewCfg()
	cfg.MaxIterations = 1
	cfg.VerificationCommands = []string{"echo verified"}
	env := recordingEnvironment(t)
	turns := 0

	meta, _, _, _ := runSelfReviewLoop(context.Background(), RunSelfReviewParams{
		Session:        domain.Session{ID: "sess"},
		Issue:          selfReviewIssue(),
		WorkspacePath:  wsPath,
		Target:         workspace.NewTarget(env, filepath.Base(wsPath), wsPath, "id", "ID-1"),
		Config:         cfg,
		AgentAdapter:   &verdictWriter{wsPath: wsPath, verdicts: []domain.ReviewVerdict{{Verdict: "pass"}}},
		OnEvent:        func(_ string, _ domain.AgentEvent) {},
		Logger:         discardLogger(),
		Metrics:        &reviewMetricsCount{},
		TurnsCompleted: &turns,
	})
	if meta == nil || len(meta.Iterations) != 1 {
		t.Fatalf("meta = %+v, want one iteration", meta)
	}

	cmds := env.Commands()
	if got := gitSubcommands(cmds); !slices.Equal(got, []string{"add", "diff"}) {
		t.Errorf("git commands run through the environment = %q, want [add diff]: the intent-to-add then the diff", got)
	}
	if got := shellScripts(cmds); !slices.Equal(got, []string{"echo verified"}) {
		t.Errorf("verification commands run through the environment = %q, want [echo verified]", got)
	}
	if meta.Iterations[0].DiffSizeBytes == 0 {
		t.Error("DiffSizeBytes = 0, want the diff produced through the environment to name new.txt")
	}
	results := meta.Iterations[0].VerificationResults
	if len(results) != 1 || results[0].ExitCode != 0 || !strings.Contains(results[0].Stdout, "verified") {
		t.Errorf("verification results = %+v, want one passing result with its output", results)
	}
}

func TestRunReactionTriage_RunsThroughEnvironmentWithHostFiles(t *testing.T) {
	t.Parallel()

	root := mustTriageWorkspace(t, "TRIAGE-ENV")
	env := recordingEnvironment(t)
	req := gateRequest(root, "TRIAGE-ENV", "fp-env")
	req.Environment = env

	outcome := RunReactionTriage(context.Background(), config.ReactionTriageConfig{
		Script:    triageVerdictScript("handled"),
		TimeoutMS: 5000,
	}, req, discardLogger())

	if outcome.Disposition != TriageHandled {
		t.Errorf("Disposition = %q (fallback %q), want %q", outcome.Disposition, outcome.Fallback, TriageHandled)
	}
	cmds := env.Commands()
	if len(cmds) != 1 {
		t.Fatalf("environment commands = %d, want 1", len(cmds))
	}
	if got := shellScripts(cmds); !slices.Equal(got, []string{triageVerdictScript("handled")}) {
		t.Errorf("triage scripts = %q, want the configured script", got)
	}
	tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	for _, name := range []string{"SORTIE_REACTION_INPUT", "SORTIE_REACTION_RESULT"} {
		value := ""
		for _, entry := range cmds[0].Spec.Env {
			if v, ok := strings.CutPrefix(entry, name+"="); ok {
				value = v
			}
		}
		if !strings.HasPrefix(value, tmp) || strings.HasPrefix(value, root) {
			t.Errorf("%s = %q, want a path in the host temporary directory %q, outside the workspace", name, value, tmp)
		}
	}
}

func TestReactionTriageGate_PassesEnvironmentToRun(t *testing.T) {
	t.Parallel()

	root := mustTriageWorkspace(t, "TRIAGE-GATE-ENV")
	state, params := gateParams(t, &triageGateStore{}, root)
	env := recordingEnvironment(t)
	params.Environment = env
	pending := &PendingReaction{}

	reactionTriageGate(state, params, pending, config.ReactionTriageConfig{
		Script:    triageVerdictScript("handled"),
		TimeoutMS: 5000,
	}, gateRequest(root, "TRIAGE-GATE-ENV", "fp-gate-env"), discardLogger(), context.Background())
	if pending.Triage == nil {
		t.Fatal("triage run not started")
	}
	<-pending.Triage.Done
	state.TriageWg.Wait()

	if got := len(env.Commands()); got != 1 {
		t.Errorf("environment commands = %d, want the triage command run through params.Environment", got)
	}
}

// carrierEnvironment is a recording environment that also carries agent
// launches, standing in for a kind that runs the agent elsewhere.
type carrierEnvironment struct {
	*environmenttest.Recorder
}

var _ environment.Carrier = carrierEnvironment{}

func (carrierEnvironment) LaunchArgv(_ environment.Workspace, launch domain.CarrierLaunch) ([]string, error) {
	return append([]string{"carrier", "--"}, launch.Argv...), nil
}

func (carrierEnvironment) ConnectionFailed(int, []string) bool { return false }

// paramsRecordingAdapter records the parameters of every session start,
// the credential check included, and delegates to the mock adapter.
type paramsRecordingAdapter struct {
	mockAgentAdapter
	mu     sync.Mutex
	params []domain.StartSessionParams
}

func (a *paramsRecordingAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	a.mu.Lock()
	a.params = append(a.params, params)
	a.mu.Unlock()
	return a.mockAgentAdapter.StartSession(ctx, params)
}

func TestRunWorkerAttempt_SessionLaunchCarrier(t *testing.T) {
	tests := []struct {
		name        string
		env         func(t *testing.T) environment.Environment
		wantCarrier bool
	}{
		{name: "local passes no carrier", env: func(t *testing.T) environment.Environment { return recordingEnvironment(t) }},
		{name: "carrier environment carries every session", env: func(t *testing.T) environment.Environment {
			return carrierEnvironment{Recorder: recordingEnvironment(t)}
		}, wantCarrier: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := defaultWorkerConfig(root)
			cfg.Agent.MaxTurns = 1
			adapter := &paramsRecordingAdapter{}
			env := tt.env(t)

			ec := newExitCapture()
			RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
				TrackerAdapter:         &mockTrackerAdapter{},
				AgentAdapter:           adapter,
				ConfigFunc:             func() config.ServiceConfig { return cfg },
				PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
				OnEvent:                func(_ string, _ domain.AgentEvent) {},
				OnExit:                 ec.onExit,
				Logger:                 discardLogger(),
				EnvironmentFunc: func(config.EnvironmentConfig) (environment.Environment, error) {
					return env, nil
				},
			})
			result := ec.waitResult(t)

			adapter.mu.Lock()
			defer adapter.mu.Unlock()
			var sawCheck, sawWork bool
			for _, p := range adapter.params {
				if p.CredentialVerification {
					sawCheck = true
				} else {
					sawWork = true
				}
				if !tt.wantCarrier {
					if p.Carrier != nil {
						t.Errorf("session (credential check %v) Carrier = %v, want nil: the agent launches as it does without an environment", p.CredentialVerification, p.Carrier)
					}
					continue
				}
				if p.Carrier == nil {
					t.Fatalf("session (credential check %v) Carrier = nil, want the environment's carrier", p.CredentialVerification)
				}
				argv, err := p.Carrier.LaunchArgv(domain.CarrierLaunch{Argv: []string{"agent"}})
				if err != nil || !slices.Equal(argv, []string{"carrier", "--", "agent"}) {
					t.Errorf("Carrier.LaunchArgv() = %q, %v, want the environment's launch", argv, err)
				}
				mcp := filepath.Join(result.WorkspacePath, ".sortie", "mcp.json")
				if got := p.Carrier.GuestPath(mcp); got != mcp {
					t.Errorf("Carrier.GuestPath(%q) = %q, want it unchanged: the recorder's guest path is the host path", mcp, got)
				}
			}
			if !sawCheck || !sawWork {
				t.Errorf("session starts = %d (credential check %v, work %v), want both", len(adapter.params), sawCheck, sawWork)
			}
		})
	}
}
