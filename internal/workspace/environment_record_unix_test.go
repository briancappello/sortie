//go:build unix

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	"github.com/sortie-ai/sortie/internal/environment/local"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// fakeVMKind is the kind name of the second environment kind these
// tests switch to.
const fakeVMKind = "fakevm"

// namedRecorder is a recording environment of a second kind whose
// workspaces run in named instances.
type namedRecorder struct {
	*environmenttest.Recorder
}

func (namedRecorder) InstanceName(ws environment.Workspace) string { return "vm-" + ws.Key }

func newFakeVM() namedRecorder {
	return namedRecorder{&environmenttest.Recorder{KindName: fakeVMKind, Inner: local.Environment{}}}
}

// existingWorkspace creates the REC-1 workspace under root with a file
// the agent left, as an earlier dispatch would.
func existingWorkspace(t *testing.T, root string) string {
	t.Helper()
	res, err := Ensure(root, "REC-1")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(res.Path, "kept.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	return res.Path
}

// kindBuilder returns an EnvironmentForKind that hands out envs by kind
// and records every kind asked for.
func kindBuilder(envs map[string]environment.Environment, asked *[]string) func(string) (environment.Environment, error) {
	return func(kind string) (environment.Environment, error) {
		*asked = append(*asked, kind)
		env, ok := envs[kind]
		if !ok {
			return nil, errors.New("unknown kind " + kind)
		}
		return env, nil
	}
}

const recordAfterCreate = "echo created > created.txt"

func prepareRecordTest(t *testing.T, root string, env environment.Environment, forKind func(string) (environment.Environment, error)) (PrepareResult, error) {
	t.Helper()
	return Prepare(context.Background(), PrepareParams{
		Root:               root,
		Identifier:         "REC-1",
		IssueID:            "id-rec-1",
		AfterCreate:        recordAfterCreate,
		HookTimeoutMS:      5000,
		Environment:        env,
		EnvironmentForKind: forKind,
	})
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	return err == nil
}

func readRecord(t *testing.T, path string) EnvironmentRecord {
	t.Helper()
	data, err := workspacekit.ReadSortieFile(path, EnvironmentRecordFile, environmentRecordMaxBytes)
	if err != nil {
		t.Fatalf("read environment record: %v", err)
	}
	var rec EnvironmentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse environment record %q: %v", data, err)
	}
	return rec
}

func TestPrepare_UnrecordedWorkspaceUnderLocalIsKept(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		env  environment.Environment
	}{
		{name: "local kind", env: local.Environment{}},
		{name: "host", env: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			path := existingWorkspace(t, root)
			var asked []string

			res, err := prepareRecordTest(t, root, tt.env, kindBuilder(nil, &asked))
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if res.CreatedNow {
				t.Error("CreatedNow = true, want the existing workspace kept")
			}
			if !fileExists(t, filepath.Join(path, "kept.txt")) {
				t.Error("kept.txt is gone, want the existing workspace kept")
			}
			if fileExists(t, filepath.Join(path, "created.txt")) {
				t.Error("after_create ran, want it skipped for a kept workspace")
			}
			if fileExists(t, filepath.Join(path, workspacekit.SortieDir)) {
				t.Error(".sortie exists, want the local kind to write no record")
			}
			if len(asked) != 0 {
				t.Errorf("EnvironmentForKind asked for %q, want no call", asked)
			}
		})
	}
}

func TestPrepare_NewWorkspaceUnderLocalWritesNoRecord(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	res, err := prepareRecordTest(t, root, local.Environment{}, nil)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !res.CreatedNow {
		t.Error("CreatedNow = false, want a new workspace")
	}
	if fileExists(t, filepath.Join(res.Path, workspacekit.SortieDir)) {
		t.Error(".sortie exists, want the local kind to write no record")
	}
}

func TestPrepare_UnrecordedWorkspaceUnderAnotherKindIsRecreated(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := existingWorkspace(t, root)
	stale := &environmenttest.Recorder{KindName: config.EnvironmentKindLocal}
	var asked []string
	vm := newFakeVM()

	res, err := prepareRecordTest(t, root, vm, kindBuilder(map[string]environment.Environment{config.EnvironmentKindLocal: stale}, &asked))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !res.CreatedNow {
		t.Error("CreatedNow = false, want the workspace prepared again")
	}
	if res.Path != path {
		t.Errorf("Path = %q, want %q", res.Path, path)
	}
	if fileExists(t, filepath.Join(path, "kept.txt")) {
		t.Error("kept.txt survived, want the stale workspace removed")
	}
	if !fileExists(t, filepath.Join(path, "created.txt")) {
		t.Error("after_create did not run, want it run for the recreated workspace")
	}
	if !slices.Equal(asked, []string{config.EnvironmentKindLocal}) {
		t.Errorf("EnvironmentForKind asked for %q, want [local]", asked)
	}
	if got, want := stale.Ops(), []environmenttest.Operation{environmenttest.OpRemove}; !slices.Equal(got, want) {
		t.Errorf("recorded kind ops = %q, want %q", got, want)
	}
	if got := stale.Calls(); len(got) == 1 && got[0].Workspace.HostPath != path {
		t.Errorf("recorded kind removed %q, want %q", got[0].Workspace.HostPath, path)
	}
	if slices.Contains(vm.Ops(), environmenttest.OpRemove) {
		t.Errorf("configured kind ops = %q, want no Remove", vm.Ops())
	}
	if got, want := readRecord(t, path), (EnvironmentRecord{Kind: fakeVMKind, Name: "vm-REC-1"}); got != want {
		t.Errorf("record = %+v, want %+v", got, want)
	}
}

func TestPrepare_WorkspaceRecordedAsConfiguredKindIsKept(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := existingWorkspace(t, root)
	if err := writeEnvironmentRecord(path, EnvironmentRecord{Kind: fakeVMKind, Name: "vm-REC-1"}); err != nil {
		t.Fatalf("writeEnvironmentRecord() error = %v", err)
	}
	var asked []string
	vm := newFakeVM()

	res, err := prepareRecordTest(t, root, vm, kindBuilder(nil, &asked))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if res.CreatedNow {
		t.Error("CreatedNow = true, want the workspace kept")
	}
	if !fileExists(t, filepath.Join(path, "kept.txt")) {
		t.Error("kept.txt is gone, want the workspace kept")
	}
	if fileExists(t, filepath.Join(path, "created.txt")) {
		t.Error("after_create ran, want it skipped")
	}
	if len(asked) != 0 || slices.Contains(vm.Ops(), environmenttest.OpRemove) {
		t.Errorf("EnvironmentForKind asked for %q, configured ops %q, want no removal", asked, vm.Ops())
	}
}

func TestPrepare_WorkspaceRecordedAsAnotherKindIsRecreatedUnderLocal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := existingWorkspace(t, root)
	if err := writeEnvironmentRecord(path, EnvironmentRecord{Kind: fakeVMKind, Name: "vm-REC-1"}); err != nil {
		t.Fatalf("writeEnvironmentRecord() error = %v", err)
	}
	vm := newFakeVM()
	var asked []string

	res, err := prepareRecordTest(t, root, local.Environment{}, kindBuilder(map[string]environment.Environment{fakeVMKind: vm}, &asked))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !res.CreatedNow {
		t.Error("CreatedNow = false, want the workspace prepared again")
	}
	if fileExists(t, filepath.Join(path, "kept.txt")) || !fileExists(t, filepath.Join(path, "created.txt")) {
		t.Error("want the stale workspace removed and after_create run")
	}
	if got, want := vm.Ops(), []environmenttest.Operation{environmenttest.OpRemove}; !slices.Equal(got, want) {
		t.Errorf("recorded kind ops = %q, want %q", got, want)
	}
	if fileExists(t, filepath.Join(path, workspacekit.SortieDir)) {
		t.Error(".sortie exists, want the local kind to write no record")
	}
}

func TestPrepare_StaleWorkspaceKeptWhenItsKindCannotRemoveIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		forKind func(t *testing.T) func(string) (environment.Environment, error)
	}{
		{
			name: "kind cannot be built",
			forKind: func(*testing.T) func(string) (environment.Environment, error) {
				var asked []string
				return kindBuilder(nil, &asked)
			},
		},
		{
			name: "no builder",
			forKind: func(*testing.T) func(string) (environment.Environment, error) {
				return nil
			},
		},
		{
			name: "removal fails",
			forKind: func(*testing.T) func(string) (environment.Environment, error) {
				failing := &environmenttest.Recorder{KindName: "oldvm", RemoveErr: errors.New("boom")}
				var asked []string
				return kindBuilder(map[string]environment.Environment{"oldvm": failing}, &asked)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			path := existingWorkspace(t, root)
			if err := writeEnvironmentRecord(path, EnvironmentRecord{Kind: "oldvm"}); err != nil {
				t.Fatalf("writeEnvironmentRecord() error = %v", err)
			}

			_, err := prepareRecordTest(t, root, newFakeVM(), tt.forKind(t))
			if err == nil {
				t.Fatal("Prepare() error = nil, want the stale workspace's removal to fail")
			}
			if !fileExists(t, filepath.Join(path, "kept.txt")) {
				t.Error("kept.txt is gone, want the workspace kept")
			}
			if got := readRecord(t, path); got.Kind != "oldvm" {
				t.Errorf("record kind = %q, want the old record kept", got.Kind)
			}
		})
	}
}

func TestPrepare_UnusableRecordRecreatesWithConfiguredKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "malformed", setup: func(t *testing.T, path string) { writeRawRecord(t, path, []byte("not json")) }},
		{name: "no kind", setup: func(t *testing.T, path string) { writeRawRecord(t, path, []byte(`{"name":"x"}`)) }},
		{name: "oversized", setup: func(t *testing.T, path string) {
			writeRawRecord(t, path, make([]byte, environmentRecordMaxBytes+1))
		}},
		{name: "link", setup: func(t *testing.T, path string) {
			outside := filepath.Join(t.TempDir(), "record")
			if err := os.WriteFile(outside, []byte(`{"kind":"fakevm"}`), 0o600); err != nil {
				t.Fatalf("write outside record: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(path, workspacekit.SortieDir), 0o700); err != nil {
				t.Fatalf("mkdir .sortie: %v", err)
			}
			mustSymlink(t, outside, filepath.Join(path, workspacekit.SortieDir, EnvironmentRecordFile))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			path := existingWorkspace(t, root)
			tt.setup(t, path)
			vm := newFakeVM()
			var asked []string

			res, err := prepareRecordTest(t, root, vm, kindBuilder(nil, &asked))
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if !res.CreatedNow || fileExists(t, filepath.Join(path, "kept.txt")) {
				t.Errorf("CreatedNow = %v, want the workspace recreated", res.CreatedNow)
			}
			if len(asked) != 0 {
				t.Errorf("EnvironmentForKind asked for %q, want the configured kind to remove", asked)
			}
			if ops := vm.Ops(); len(ops) == 0 || ops[0] != environmenttest.OpRemove {
				t.Errorf("configured kind ops = %q, want Remove first", ops)
			}
			if got := readRecord(t, path); got.Kind != fakeVMKind {
				t.Errorf("record kind = %q, want %q", got.Kind, fakeVMKind)
			}
		})
	}
}

func writeRawRecord(t *testing.T, path string, data []byte) {
	t.Helper()
	dir := filepath.Join(path, workspacekit.SortieDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .sortie: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, EnvironmentRecordFile), data, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
}
