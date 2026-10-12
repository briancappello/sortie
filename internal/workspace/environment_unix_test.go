//go:build unix

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	"github.com/sortie-ai/sortie/internal/environment/local"
)

// recordingEnvironment returns a recorder that runs every command as the
// local kind would, so a hook still leaves its effects in the host
// workspace while the test sees that it went through the environment.
func recordingEnvironment() *environmenttest.Recorder {
	return &environmenttest.Recorder{Inner: local.Environment{}}
}

// wantShellCommand fails the test unless call is an environment command
// that runs script through "sh -c" in the workspace root of wantPath.
func wantShellCommand(t *testing.T, call environmenttest.Call, script, wantPath string) {
	t.Helper()
	if call.Op != environmenttest.OpCommand {
		t.Fatalf("call op = %q, want %q", call.Op, environmenttest.OpCommand)
	}
	if want := []string{"sh", "-c", script}; !slices.Equal(call.Spec.Argv, want) {
		t.Errorf("command argv = %q, want %q", call.Spec.Argv, want)
	}
	if call.Spec.Dir != "" {
		t.Errorf("command dir = %q, want the workspace root", call.Spec.Dir)
	}
	if call.Workspace.HostPath != wantPath {
		t.Errorf("command workspace host path = %q, want %q", call.Workspace.HostPath, wantPath)
	}
	if !slices.Contains(call.Spec.Env, "SORTIE_WORKSPACE="+wantPath) {
		t.Errorf("command env = %q, want SORTIE_WORKSPACE=%s", call.Spec.Env, wantPath)
	}
}

func TestPrepare_RunsLifecycleThroughEnvironment(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	env := recordingEnvironment()
	afterCreate := "echo created > created.txt"
	beforeRun := "echo ran > ran.txt"

	res, err := Prepare(context.Background(), PrepareParams{
		Root:          root,
		Identifier:    "ENV-1",
		IssueID:       "id-env-1",
		AfterCreate:   afterCreate,
		BeforeRun:     beforeRun,
		HookTimeoutMS: 5000,
		Environment:   env,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	wantOps := []environmenttest.Operation{environmenttest.OpPrepare, environmenttest.OpCommand, environmenttest.OpAfterCreate, environmenttest.OpCommand}
	if got := env.Ops(); !slices.Equal(got, wantOps) {
		t.Fatalf("environment ops = %q, want %q", got, wantOps)
	}
	calls := env.Calls()
	wantWS := environment.Workspace{Key: "ENV-1", HostPath: res.Path, GuestPath: res.Path, IssueID: "id-env-1", Identifier: "ENV-1"}
	if calls[0].Workspace != wantWS {
		t.Errorf("Prepare workspace = %+v, want %+v", calls[0].Workspace, wantWS)
	}
	wantShellCommand(t, calls[1], afterCreate, res.Path)
	wantShellCommand(t, calls[3], beforeRun, res.Path)

	for _, name := range []string{"created.txt", "ran.txt"} {
		if _, statErr := os.Stat(filepath.Join(res.Path, name)); statErr != nil {
			t.Errorf("hook output %s: %v, want the local inner environment to run the hook in the workspace", name, statErr)
		}
	}
}

func TestPrepare_EnvironmentCreatedNowRunsAfterCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		envCreatedNow   bool
		wantAfterCreate bool
	}{
		{name: "environment created now", envCreatedNow: true, wantAfterCreate: true},
		{name: "environment reused", envCreatedNow: false, wantAfterCreate: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			existing, err := Ensure(root, "ENV-2")
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			env := recordingEnvironment()
			// The existing workspace was created by this kind, so it is
			// reused rather than recreated as a stale one.
			if err := writeEnvironmentRecord(existing.Path, EnvironmentRecord{Kind: env.Kind()}); err != nil {
				t.Fatalf("writeEnvironmentRecord() error = %v", err)
			}
			env.PrepareResult = environment.PrepareResult{CreatedNow: tt.envCreatedNow}

			res, err := Prepare(context.Background(), PrepareParams{
				Root:          root,
				Identifier:    "ENV-2",
				IssueID:       "id-env-2",
				AfterCreate:   "echo created > created.txt",
				HookTimeoutMS: 5000,
				Environment:   env,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if res.CreatedNow != tt.wantAfterCreate {
				t.Errorf("CreatedNow = %v, want %v", res.CreatedNow, tt.wantAfterCreate)
			}
			ran := len(env.Commands()) == 1
			if ran != tt.wantAfterCreate {
				t.Errorf("after_create ran = %v (ops %q), want %v", ran, env.Ops(), tt.wantAfterCreate)
			}
			finished := slices.Contains(env.Ops(), environmenttest.OpAfterCreate)
			if finished != tt.wantAfterCreate {
				t.Errorf("AfterCreate called = %v, want %v", finished, tt.wantAfterCreate)
			}
		})
	}
}

func TestPrepare_EnvironmentFailureRunsNoHook(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	prepErr := errors.New("vm failed to start")
	env := recordingEnvironment()
	env.PrepareErr = prepErr

	_, err := Prepare(context.Background(), PrepareParams{
		Root:          root,
		Identifier:    "ENV-3",
		IssueID:       "id-env-3",
		AfterCreate:   "echo created > created.txt",
		BeforeRun:     "echo ran > ran.txt",
		HookTimeoutMS: 5000,
		Environment:   env,
	})
	if !errors.Is(err, prepErr) {
		t.Fatalf("Prepare() error = %v, want it to wrap %v", err, prepErr)
	}
	if got := env.Ops(); !slices.Equal(got, []environmenttest.Operation{environmenttest.OpPrepare}) {
		t.Errorf("environment ops = %q, want only Prepare: no hook may run", got)
	}
	if _, statErr := os.Stat(filepath.Join(root, "ENV-3")); !os.IsNotExist(statErr) {
		t.Errorf("workspace directory stat error = %v, want the freshly created directory rolled back", statErr)
	}
}

func TestPrepare_AfterCreateFinisherFailureRollsBack(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	finishErr := errors.New("link failed")
	env := recordingEnvironment()
	env.AfterCreateErr = finishErr

	_, err := Prepare(context.Background(), PrepareParams{
		Root:          root,
		Identifier:    "ENV-4",
		IssueID:       "id-env-4",
		AfterCreate:   "true",
		BeforeRun:     "echo ran > ran.txt",
		HookTimeoutMS: 5000,
		Environment:   env,
	})
	if !errors.Is(err, finishErr) {
		t.Fatalf("Prepare() error = %v, want it to wrap %v", err, finishErr)
	}
	if got := len(env.Commands()); got != 1 {
		t.Errorf("commands run = %d, want 1: before_run must not run after a failed AfterCreate", got)
	}
	if _, statErr := os.Stat(filepath.Join(root, "ENV-4")); !os.IsNotExist(statErr) {
		t.Errorf("workspace directory stat error = %v, want it rolled back", statErr)
	}
}

func TestFinish_RunsAfterRunThroughEnvironment(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ws, err := Ensure(root, "ENV-5")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	env := recordingEnvironment()
	afterRun := "echo done > done.txt"

	Finish(context.Background(), FinishParams{
		Path:          ws.Path,
		Identifier:    "ENV-5",
		IssueID:       "id-env-5",
		AfterRun:      afterRun,
		HookTimeoutMS: 5000,
		Environment:   env,
	})

	cmds := env.Commands()
	if len(cmds) != 1 {
		t.Fatalf("commands = %d (ops %q), want 1", len(cmds), env.Ops())
	}
	wantShellCommand(t, cmds[0], afterRun, ws.Path)
	if cmds[0].Workspace.Key != "ENV-5" {
		t.Errorf("workspace key = %q, want ENV-5", cmds[0].Workspace.Key)
	}
	if _, statErr := os.Stat(filepath.Join(ws.Path, "done.txt")); statErr != nil {
		t.Errorf("after_run output: %v", statErr)
	}
}

// removalCase runs one workspace removal call site against env and
// returns the workspace path it removed.
type removalCase struct {
	name   string
	remove func(t *testing.T, root string, env environment.Environment) (string, error)
}

func removalCases() []removalCase {
	const beforeRemove = "echo removing > removing.txt"
	return []removalCase{
		{
			name: "Cleanup",
			remove: func(t *testing.T, root string, env environment.Environment) (string, error) {
				ws, err := Ensure(root, "ENV-6")
				if err != nil {
					t.Fatalf("Ensure() error = %v", err)
				}
				return ws.Path, Cleanup(context.Background(), CleanupParams{
					Root: root, Identifier: "ENV-6", IssueID: "id-env-6",
					BeforeRemove: beforeRemove, HookTimeoutMS: 5000, Environment: env,
				})
			},
		},
		{
			name: "CleanupByPath",
			remove: func(t *testing.T, root string, env environment.Environment) (string, error) {
				ws, err := Ensure(root, "ENV-6")
				if err != nil {
					t.Fatalf("Ensure() error = %v", err)
				}
				return ws.Path, CleanupByPath(context.Background(), CleanupByPathParams{
					Path: ws.Path, Identifier: "ENV-6", IssueID: "id-env-6",
					BeforeRemove: beforeRemove, HookTimeoutMS: 5000, Environment: env,
				})
			},
		},
		{
			name: "CleanupTerminal",
			remove: func(t *testing.T, root string, env environment.Environment) (string, error) {
				ws, err := Ensure(root, "ENV-6")
				if err != nil {
					t.Fatalf("Ensure() error = %v", err)
				}
				res := CleanupTerminal(context.Background(), CleanupTerminalParams{
					Root: root, Identifiers: []string{"ENV-6"}, IssueIDsByIdentifier: map[string]string{"ENV-6": "id-env-6"},
					BeforeRemove: beforeRemove, HookTimeoutMS: 5000, Environment: env,
				})
				return ws.Path, res.Errors["ENV-6"]
			},
		},
	}
}

func TestCleanup_RunsBeforeRemoveThenRemovesEnvironment(t *testing.T) {
	t.Parallel()

	for _, tc := range removalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			env := recordingEnvironment()
			path, err := tc.remove(t, root, env)
			if err != nil {
				t.Fatalf("removal error = %v", err)
			}

			wantOps := []environmenttest.Operation{environmenttest.OpCommand, environmenttest.OpRemove}
			if got := env.Ops(); !slices.Equal(got, wantOps) {
				t.Fatalf("environment ops = %q, want %q: before_remove in the environment, then its removal", got, wantOps)
			}
			calls := env.Calls()
			wantShellCommand(t, calls[0], "echo removing > removing.txt", path)
			wantWS := environment.Workspace{Key: "ENV-6", HostPath: path, GuestPath: path, IssueID: "id-env-6", Identifier: "ENV-6"}
			if calls[1].Workspace != wantWS {
				t.Errorf("Remove workspace = %+v, want %+v", calls[1].Workspace, wantWS)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Errorf("workspace stat error = %v, want the directory removed", statErr)
			}
		})
	}
}

func TestCleanup_EnvironmentRemovalFailureKeepsWorkspace(t *testing.T) {
	t.Parallel()

	for _, tc := range removalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			removeErr := errors.New("vm delete failed")
			env := recordingEnvironment()
			env.RemoveErr = removeErr
			path, err := tc.remove(t, root, env)
			if !errors.Is(err, removeErr) {
				t.Fatalf("removal error = %v, want it to wrap %v", err, removeErr)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Errorf("workspace stat error = %v, want the directory kept for a later retry", statErr)
			}
		})
	}
}

func TestRunHook_DirOutsideTargetWorkspaceFailsToStart(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	env := recordingEnvironment()
	_, err := RunHook(context.Background(), HookParams{
		Script:    "true",
		Dir:       t.TempDir(),
		TimeoutMS: 5000,
		Target:    NewTarget(env, filepath.Base(ws), ws, "id", "ID-1"),
	})
	var he *HookError
	if !errors.As(err, &he) || he.Op != "start" {
		t.Fatalf("RunHook() error = %v, want a *HookError with Op start", err)
	}
	if got := env.Ops(); len(got) != 0 {
		t.Errorf("environment ops = %q, want none", got)
	}
}

func TestHandoffEvidence_RunsGitThroughEnvironment(t *testing.T) {
	t.Parallel()

	dir := newHandoffEvidenceGitWorkspace(t)
	hostBaseline, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("host CaptureHandoffEvidenceBaseline() error = %v", err)
	}

	env := recordingEnvironment()
	target := NewTarget(env, filepath.Base(dir), dir, "id", "ID-1")

	baseline, err := target.CaptureHandoffEvidenceBaseline(context.Background())
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline() error = %v", err)
	}
	if baseline != hostBaseline {
		t.Errorf("baseline through the local environment = %+v, want the host baseline %+v", baseline, hostBaseline)
	}
	captureCmds := env.Commands()
	assertGitThroughEnvironment(t, captureCmds)

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatalf("modify tracked file: %v", err)
	}
	change, err := target.CompareHandoffEvidenceBaseline(context.Background(), baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline() error = %v", err)
	}
	if !change.WorktreeChanged {
		t.Errorf("WorktreeChanged = false, want true after modifying a tracked file")
	}
	compareCmds := env.Commands()[len(captureCmds):]
	assertGitThroughEnvironment(t, compareCmds)
}

// assertGitThroughEnvironment fails the test unless cmds holds the
// inspection's git commands, each run in the workspace root with
// optional locks off.
func assertGitThroughEnvironment(t *testing.T, cmds []environmenttest.Call) {
	t.Helper()
	if len(cmds) < 5 {
		t.Fatalf("environment commands = %d, want every git inspection command (at least 5)", len(cmds))
	}
	for _, c := range cmds {
		if len(c.Spec.Argv) == 0 || c.Spec.Argv[0] != "git" {
			t.Errorf("command argv = %q, want a git command", c.Spec.Argv)
		}
		if c.Spec.Dir != "" {
			t.Errorf("command dir = %q, want the workspace root", c.Spec.Dir)
		}
		if !slices.Contains(c.Spec.Env, "GIT_OPTIONAL_LOCKS=0") {
			t.Errorf("command %q env lacks GIT_OPTIONAL_LOCKS=0", c.Spec.Argv)
		}
	}
}
