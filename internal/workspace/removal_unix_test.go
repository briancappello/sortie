//go:build unix

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	"github.com/sortie-ai/sortie/internal/environment/local"
)

const removalHook = "echo removing > removing.txt"

// kindRemovalCase runs one workspace removal call site with a
// configured environment and a builder for other kinds, on the RM-1
// workspace under root, which the test created.
type kindRemovalCase struct {
	name   string
	remove func(root, path string, env environment.Environment, forKind func(string) (environment.Environment, error)) error
}

func kindRemovalCases() []kindRemovalCase {
	return []kindRemovalCase{
		{name: "Cleanup", remove: func(root, _ string, env environment.Environment, forKind func(string) (environment.Environment, error)) error {
			return Cleanup(context.Background(), CleanupParams{
				Root: root, Identifier: "RM-1", IssueID: "id-rm-1", BeforeRemove: removalHook, HookTimeoutMS: 5000,
				Environment: env, EnvironmentForKind: forKind,
			})
		}},
		{name: "CleanupByPath", remove: func(_, path string, env environment.Environment, forKind func(string) (environment.Environment, error)) error {
			return CleanupByPath(context.Background(), CleanupByPathParams{
				Path: path, Identifier: "RM-1", IssueID: "id-rm-1", BeforeRemove: removalHook, HookTimeoutMS: 5000,
				Environment: env, EnvironmentForKind: forKind,
			})
		}},
		{name: "CleanupTerminal", remove: func(root, _ string, env environment.Environment, forKind func(string) (environment.Environment, error)) error {
			res := CleanupTerminal(context.Background(), CleanupTerminalParams{
				Root: root, Identifiers: []string{"RM-1"}, BeforeRemove: removalHook, HookTimeoutMS: 5000,
				Environment: env, EnvironmentForKind: forKind,
			})
			return res.Errors["RM-1"]
		}},
	}
}

// removalWorkspace creates the RM-1 workspace under root, with the
// environment record rec when rec.Kind is set.
func removalWorkspace(t *testing.T, root string, rec EnvironmentRecord) string {
	t.Helper()
	res, err := Ensure(root, "RM-1")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if rec.Kind != "" {
		if err := writeEnvironmentRecord(res.Path, rec); err != nil {
			t.Fatalf("write record: %v", err)
		}
	}
	return res.Path
}

func TestCleanup_RemovesWithTheRecordedKind(t *testing.T) {
	t.Parallel()

	for _, tc := range kindRemovalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			path := removalWorkspace(t, root, EnvironmentRecord{Kind: fakeVMKind, Name: "vm-RM-1"})
			configured := &environmenttest.Recorder{KindName: config.EnvironmentKindLocal, Inner: local.Environment{}}
			vm := newFakeVM()
			var asked []string

			err := tc.remove(root, path, configured, kindBuilder(map[string]environment.Environment{fakeVMKind: vm}, &asked))
			if err != nil {
				t.Fatalf("removal error = %v", err)
			}
			if !slices.Equal(asked, []string{fakeVMKind}) {
				t.Errorf("EnvironmentForKind asked for %q, want [%s]", asked, fakeVMKind)
			}
			if got := configured.Ops(); len(got) != 0 {
				t.Errorf("configured environment ops = %q, want none", got)
			}
			wantOps := []environmenttest.Operation{environmenttest.OpCommand, environmenttest.OpRemove}
			if got := vm.Ops(); !slices.Equal(got, wantOps) {
				t.Fatalf("recorded kind's ops = %q, want %q: before_remove, then the removal", got, wantOps)
			}
			wantShellCommand(t, vm.Calls()[0], removalHook, path)
			if fileExists(t, path) {
				t.Error("workspace directory still exists, want it removed after the environment")
			}
		})
	}
}

func TestCleanup_RecordNeverMovesRemovalToTheHost(t *testing.T) {
	t.Parallel()

	records := map[string]EnvironmentRecord{
		"no record":           {},
		"record naming local": {Kind: config.EnvironmentKindLocal},
		"own kind":            {Kind: fakeVMKind, Name: "vm-RM-1"},
	}
	for _, tc := range kindRemovalCases() {
		for recName, rec := range records {
			t.Run(tc.name+"/"+recName, func(t *testing.T) {
				t.Parallel()

				root := t.TempDir()
				path := removalWorkspace(t, root, rec)
				configured := newFakeVM()
				var asked []string

				err := tc.remove(root, path, configured, kindBuilder(map[string]environment.Environment{
					config.EnvironmentKindLocal: local.Environment{},
				}, &asked))
				if err != nil {
					t.Fatalf("removal error = %v", err)
				}
				if len(asked) != 0 {
					t.Errorf("EnvironmentForKind asked for %q, want no call: the configured kind removes it", asked)
				}
				wantOps := []environmenttest.Operation{environmenttest.OpCommand, environmenttest.OpRemove}
				if got := configured.Ops(); !slices.Equal(got, wantOps) {
					t.Errorf("configured environment ops = %q, want %q", got, wantOps)
				}
			})
		}
	}
}

func TestCleanup_UnbuildableRecordedKindKeepsWorkspace(t *testing.T) {
	t.Parallel()

	for _, tc := range kindRemovalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			path := removalWorkspace(t, root, EnvironmentRecord{Kind: "gone-kind"})
			configured := recordingEnvironment()
			var asked []string

			err := tc.remove(root, path, configured, kindBuilder(nil, &asked))
			if err == nil {
				t.Fatal("removal error = nil, want the build failure")
			}
			if got := configured.Ops(); len(got) != 0 {
				t.Errorf("configured environment ops = %q, want none", got)
			}
			if !fileExists(t, path) {
				t.Error("workspace directory removed, want it kept: its instance may still exist")
			}
		})
	}
}

func TestCleanup_ResumesTheInstanceForBeforeRemove(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		hook      string
		ready     bool
		resumeErr error
		wantOps   []environmenttest.Operation
		wantHook  bool
	}{
		{name: "instance resumed", hook: removalHook, ready: true,
			wantOps:  []environmenttest.Operation{environmenttest.OpResume, environmenttest.OpCommand, environmenttest.OpRemove},
			wantHook: true},
		{name: "no instance", hook: removalHook,
			wantOps: []environmenttest.Operation{environmenttest.OpResume, environmenttest.OpRemove}},
		{name: "resume fails", hook: removalHook, resumeErr: errors.New("start failed"),
			wantOps: []environmenttest.Operation{environmenttest.OpResume, environmenttest.OpRemove}},
		{name: "no hook", ready: true,
			wantOps: []environmenttest.Operation{environmenttest.OpRemove}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			path := removalWorkspace(t, root, EnvironmentRecord{})
			rec := &environmenttest.Recorder{KindName: fakeVMKind, Inner: local.Environment{}}
			env := &environmenttest.ResumingRecorder{Recorder: rec, Ready: tt.ready, ResumeErr: tt.resumeErr}
			var hookRan bool

			// The hook's marker is checked before the directory goes, so
			// a command wrapper records it.
			err := CleanupByPath(context.Background(), CleanupByPathParams{
				Path: path, Identifier: "RM-1", IssueID: "id-rm-1", BeforeRemove: tt.hook, HookTimeoutMS: 5000,
				Environment: hookObserver{ResumingRecorder: env, ran: &hookRan, path: path},
			})
			if err != nil {
				t.Fatalf("CleanupByPath() error = %v", err)
			}
			if got := rec.Ops(); !slices.Equal(got, tt.wantOps) {
				t.Errorf("environment ops = %q, want %q", got, tt.wantOps)
			}
			if hookRan != tt.wantHook {
				t.Errorf("before_remove ran = %v, want %v", hookRan, tt.wantHook)
			}
			if fileExists(t, path) {
				t.Error("workspace directory still exists, want it removed")
			}
		})
	}
}

// hookObserver records whether the before_remove hook left its marker
// in the workspace by the time the environment is removed.
type hookObserver struct {
	*environmenttest.ResumingRecorder
	ran  *bool
	path string
}

func (h hookObserver) Remove(ctx context.Context, ws environment.Workspace) error {
	if _, err := os.Stat(filepath.Join(h.path, "removing.txt")); err == nil {
		*h.ran = true
	}
	return h.ResumingRecorder.Remove(ctx, ws)
}
