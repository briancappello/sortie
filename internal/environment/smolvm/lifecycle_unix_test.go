//go:build unix

package smolvm

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

// ownedVM is the VM the fixture's environment created for key, in state.
func (f prepareFixture) ownedVM(key, state string) fakeMachine {
	return fakeMachine{
		Name:  VMName(key),
		State: state,
		Labels: map[string]string{
			LabelManaged:   "1",
			LabelWorkspace: key,
			LabelPack:      sha256Hex(testPackContent),
			LabelWorkflow:  shortHash(f.workflow),
		},
	}
}

// stateKeyDir returns the fixture's state directory for key, created
// with a file in it.
func (f prepareFixture) stateKeyDir(t *testing.T, key string) string {
	t.Helper()
	dir := filepath.Join(f.stateDir, "env", key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tools.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f prepareFixture) machineNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, m := range f.fake.state().Machines {
		names = append(names, m.Name)
	}
	return names
}

func assertArgvs(t *testing.T, f prepareFixture, want ...[]string) {
	t.Helper()
	if got := f.fake.argvs(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("calls:\n got %q\nwant %q", got, want)
	}
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if got := err == nil; got != want {
		t.Errorf("%s exists = %v (stat error %v), want %v", path, got, err, want)
	}
}

func TestRelease_StopsTheRunningVM(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{Machines: []fakeMachine{f.ownedVM("VM-1", StateRunning)}})

	if err := f.env.Release(context.Background(), f.ws); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	assertArgvs(t, f, listArgv, []string{"machine", "stop", "--error-format", "json", "--name", "sortie-VM-1"})
	machines := f.fake.state().Machines
	if len(machines) != 1 || machines[0].State != StateStopped {
		t.Errorf("machines = %+v, want sortie-VM-1 stopped and still present", machines)
	}
}

func TestRelease_NothingToStop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		machines func(f prepareFixture) []fakeMachine
	}{
		{name: "no VM", machines: func(prepareFixture) []fakeMachine { return nil }},
		{name: "stopped VM", machines: func(f prepareFixture) []fakeMachine {
			return []fakeMachine{f.ownedVM("VM-1", StateStopped)}
		}},
		{name: "VM of another workflow", machines: func(f prepareFixture) []fakeMachine {
			vm := f.ownedVM("VM-1", StateRunning)
			vm.Labels[LabelWorkflow] = "0123456789ab"
			return []fakeMachine{vm}
		}},
		{name: "unlabeled machine with the VM's name", machines: func(prepareFixture) []fakeMachine {
			return []fakeMachine{{Name: "sortie-VM-1", State: StateRunning, Labels: map[string]string{"sortie.test": "1"}}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPrepareFixture(t, fakeState{}, nil)
			f.fake.setState(fakeState{Machines: tt.machines(f)})

			if err := f.env.Release(context.Background(), f.ws); err != nil {
				t.Fatalf("Release() error = %v, want nil", err)
			}
			assertArgvs(t, f, listArgv)
		})
	}
}

func TestResume(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		state     string // "" for no VM
		wantReady bool
		wantArgvs [][]string
	}{
		{name: "stopped VM starts", state: StateStopped, wantReady: true,
			wantArgvs: append([][]string{listArgv, {"machine", "start", "--error-format", "json", "--name", "sortie-VM-1"}}, toolListenerCalls()...)},
		{name: "running VM", state: StateRunning, wantReady: true, wantArgvs: [][]string{listArgv}},
		{name: "no VM", wantArgvs: [][]string{listArgv}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPrepareFixture(t, fakeState{}, nil)
			if tt.state != "" {
				f.fake.setState(fakeState{Machines: []fakeMachine{f.ownedVM("VM-1", tt.state)}})
			}

			ready, err := f.env.Resume(context.Background(), f.ws)
			if err != nil || ready != tt.wantReady {
				t.Fatalf("Resume() = %v, %v, want %v, nil", ready, err, tt.wantReady)
			}
			assertArgvs(t, f, tt.wantArgvs...)
			if tt.state == StateStopped {
				assertExists(t, filepath.Join(f.stateDir, "env", "VM-1"), true)
			}
		})
	}
}

func TestRemove_DeletesTheVMThenItsStateDirectory(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{Machines: []fakeMachine{f.ownedVM("VM-1", StateRunning), f.ownedVM("VM-2", StateStopped)}})
	keyDir := f.stateKeyDir(t, "VM-1")
	otherDir := f.stateKeyDir(t, "VM-2")

	if err := f.env.Remove(context.Background(), f.ws); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	assertArgvs(t, f, listArgv, []string{"machine", "delete", "--error-format", "json", "-f", "--name", "sortie-VM-1"})
	if got := f.machineNames(t); !slices.Equal(got, []string{"sortie-VM-2"}) {
		t.Errorf("machines = %q, want only sortie-VM-2 left", got)
	}
	assertExists(t, keyDir, false)
	assertExists(t, otherDir, true)
	assertExists(t, f.ws.HostPath, true) // the workspace lifecycle removes it next
}

func TestRemove_WithoutVM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fail     map[string]fakeFailure
		machines func(f prepareFixture) []fakeMachine
		wantArgv [][]string
	}{
		{
			name:     "no VM",
			machines: func(prepareFixture) []fakeMachine { return nil },
			wantArgv: [][]string{listArgv},
		},
		{
			name:     "VM gone before the delete",
			fail:     map[string]fakeFailure{"machine delete": {Exit: 1, Stderr: `{"error":{"code":"NOT_FOUND","message":"vm not found: sortie-VM-1"}}` + "\n"}},
			machines: func(f prepareFixture) []fakeMachine { return []fakeMachine{f.ownedVM("VM-1", StateStopped)} },
			wantArgv: [][]string{listArgv, {"machine", "delete", "--error-format", "json", "-f", "--name", "sortie-VM-1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newPrepareFixture(t, fakeState{}, nil)
			f.fake.setState(fakeState{Machines: tt.machines(f), Fail: tt.fail})
			keyDir := f.stateKeyDir(t, "VM-1")

			if err := f.env.Remove(context.Background(), f.ws); err != nil {
				t.Fatalf("Remove() error = %v, want nil", err)
			}
			assertArgvs(t, f, tt.wantArgv...)
			assertExists(t, keyDir, false)
		})
	}
}

func TestRemove_FailedDeleteKeepsTheStateDirectory(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{
		Machines: []fakeMachine{f.ownedVM("VM-1", StateRunning)},
		Fail:     map[string]fakeFailure{"machine delete": {Exit: 1, Stderr: "Error: database is locked\n"}},
	})
	keyDir := f.stateKeyDir(t, "VM-1")

	err := f.env.Remove(context.Background(), f.ws)
	if err == nil || IsNotFound(err) {
		t.Fatalf("Remove() error = %v, want the delete failure", err)
	}
	assertExists(t, keyDir, true)
}

func TestRemove_LeavesForeignMachinesAlone(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	other := f.ownedVM("VM-1", StateRunning)
	other.Labels[LabelWorkflow] = "0123456789ab"
	f.fake.setState(fakeState{Machines: []fakeMachine{other}})
	keyDir := f.stateKeyDir(t, "VM-1")

	if err := f.env.Remove(context.Background(), f.ws); err != nil {
		t.Fatalf("Remove() error = %v, want nil", err)
	}
	assertArgvs(t, f, listArgv)
	assertExists(t, keyDir, true)
}

func TestRemove_RefusesAKeyThatIsNotSanitized(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	for _, key := range []string{"", ".", "..", "../VM-1", "a/b", "a b"} {
		ws := f.ws
		ws.Key = key
		if err := f.env.Remove(context.Background(), ws); err == nil {
			t.Errorf("Remove(key %q) error = nil, want a refusal", key)
		}
	}
	if calls := f.fake.argvs(); len(calls) != 0 {
		t.Errorf("calls = %q, want none", calls)
	}
}

func TestSweep_SelectsLeftoverVMs(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	root := filepath.Dir(f.ws.HostPath)
	leftover := f.ownedVM("GONE-1", StateStopped)
	present := f.ownedVM("VM-1", StateStopped)
	running := f.ownedVM("RUN-1", StateRunning)
	otherWorkflow := f.ownedVM("OTHER-1", StateStopped)
	otherWorkflow.Labels[LabelWorkflow] = "0123456789ab"
	unmanaged := f.ownedVM("UNM-1", StateStopped)
	delete(unmanaged.Labels, LabelManaged)
	unlabeled := fakeMachine{Name: "sortie-UNL-1", State: StateRunning, Labels: map[string]string{"sortie.test": "1"}}
	traversal := f.ownedVM("x", StateStopped)
	traversal.Name = VMName("../GONE-1")
	traversal.Labels[LabelWorkspace] = "../GONE-1"
	mismatched := f.ownedVM("MIS-1", StateStopped)
	mismatched.Labels[LabelWorkspace] = "GONE-2"
	f.fake.setState(fakeState{Machines: []fakeMachine{
		leftover, present, running, otherWorkflow, unmanaged, unlabeled, traversal, mismatched,
	}})
	leftoverDir := f.stateKeyDir(t, "GONE-1")
	presentDir := f.stateKeyDir(t, "VM-1")

	err := f.env.Sweep(context.Background(), environment.SweepParams{
		WorkspaceRoot: root,
		Running:       map[string]bool{"RUN-1": true},
	})
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	assertArgvs(t, f, listArgv, []string{"machine", "delete", "--error-format", "json", "-f", "--name", "sortie-GONE-1"})
	want := []string{
		present.Name, running.Name, otherWorkflow.Name, unmanaged.Name, unlabeled.Name, traversal.Name, mismatched.Name,
	}
	if got := f.machineNames(t); !slices.Equal(got, want) {
		t.Errorf("machines = %q, want %q", got, want)
	}
	assertExists(t, leftoverDir, false)
	assertExists(t, presentDir, true)
}

func TestSweep_ContinuesPastAFailedDelete(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{
		Machines: []fakeMachine{f.ownedVM("GONE-1", StateStopped), f.ownedVM("GONE-2", StateStopped)},
		Fail:     map[string]fakeFailure{"machine delete": {Exit: 1, Stderr: "Error: database is locked\n"}},
	})
	keyDir := f.stateKeyDir(t, "GONE-1")

	err := f.env.Sweep(context.Background(), environment.SweepParams{WorkspaceRoot: filepath.Dir(f.ws.HostPath)})
	if err == nil {
		t.Fatal("Sweep() error = nil, want the delete failures")
	}
	assertArgvs(t, f, listArgv,
		[]string{"machine", "delete", "--error-format", "json", "-f", "--name", "sortie-GONE-1"},
		[]string{"machine", "delete", "--error-format", "json", "-f", "--name", "sortie-GONE-2"})
	assertExists(t, keyDir, true)
}

func TestSweep_RefusesARelativeRoot(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	if err := f.env.Sweep(context.Background(), environment.SweepParams{WorkspaceRoot: "workspaces"}); err == nil {
		t.Fatal("Sweep() error = nil, want a refusal of the relative root")
	}
	if calls := f.fake.argvs(); len(calls) != 0 {
		t.Errorf("calls = %q, want none", calls)
	}
}

func TestSweep_SkipsAMissingRoot(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{Machines: []fakeMachine{f.ownedVM("GONE-1", StateStopped)}})
	root := filepath.Join(t.TempDir(), "unmounted")

	if err := f.env.Sweep(context.Background(), environment.SweepParams{WorkspaceRoot: root}); err != nil {
		t.Fatalf("Sweep() error = %v, want nil", err)
	}
	if calls := f.fake.argvs(); len(calls) != 0 {
		t.Errorf("calls = %q, want none: a missing root does not mean every workspace was removed", calls)
	}
}

func TestSweepUnselected(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{Machines: []fakeMachine{f.ownedVM("GONE-1", StateStopped)}})
	params := environment.Params{
		Config:       config.EnvironmentConfig{Kind: config.EnvironmentKindSmolvm, Smolvm: config.SmolvmConfig{Binary: f.fake.Binary}},
		StateDir:     f.stateDir,
		WorkflowPath: f.workflow,
		Logger:       slog.New(slog.DiscardHandler),
	}
	p := environment.SweepParams{WorkspaceRoot: filepath.Dir(f.ws.HostPath)}

	if err := SweepUnselected(context.Background(), params, p); err != nil {
		t.Fatalf("SweepUnselected() without a state directory error = %v", err)
	}
	if calls := f.fake.argvs(); len(calls) != 0 {
		t.Fatalf("calls = %q, want none: no VM was ever created from this state directory", calls)
	}

	keyDir := f.stateKeyDir(t, "GONE-1")
	if err := SweepUnselected(context.Background(), params, p); err != nil {
		t.Fatalf("SweepUnselected() error = %v", err)
	}
	assertArgvs(t, f, listArgv, []string{"machine", "delete", "--error-format", "json", "-f", "--name", "sortie-GONE-1"})
	assertExists(t, keyDir, false)
}
