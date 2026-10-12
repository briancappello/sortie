//go:build unix

package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	"github.com/sortie-ai/sortie/internal/environment/local"
)

// guestDirEnvironment runs every command in dir instead of the host
// workspace, the way a guest environment runs it in its own copy of
// the workspace, and reports dir as the guest path.
type guestDirEnvironment struct {
	local.Environment
	dir string
}

func (e guestDirEnvironment) GuestPath(string) string { return e.dir }

func (e guestDirEnvironment) Command(ctx context.Context, ws environment.Workspace, spec environment.CommandSpec) (*exec.Cmd, error) {
	ws.HostPath = e.dir
	return e.Environment.Command(ctx, ws, spec)
}

// TestHandoffEvidence_UntrackedFileHashedInEnvironment pins that an
// untracked file's content enters the fingerprint through hash-object
// run in the environment, with the paths on its standard input: a
// change to the file the environment sees changes the fingerprint
// while the host workspace stays as it was.
func TestHandoffEvidence_UntrackedFileHashedInEnvironment(t *testing.T) {
	t.Parallel()

	hostDir := t.TempDir()
	guestDir := newHandoffEvidenceGitWorkspace(t)
	guestFile := filepath.Join(guestDir, "notes.txt")
	if err := os.WriteFile(guestFile, []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	env := &environmenttest.Recorder{
		Inner:         guestDirEnvironment{dir: guestDir},
		GuestPathFunc: func(string) string { return guestDir },
	}
	target := NewTarget(env, filepath.Base(hostDir), hostDir, "id", "ID-1")

	baseline, err := target.CaptureHandoffEvidenceBaseline(context.Background())
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline() error = %v", err)
	}

	var hashObject []environmenttest.Call
	for _, c := range env.Commands() {
		if slices.Contains(c.Spec.Argv, "hash-object") {
			hashObject = append(hashObject, c)
		}
	}
	if len(hashObject) != 1 {
		t.Fatalf("hash-object commands = %d, want 1", len(hashObject))
	}
	want := []string{"git", "--no-optional-locks", "hash-object", "--no-filters", "--stdin-paths"}
	if got := hashObject[0].Spec.Argv; !slices.Equal(got, want) {
		t.Errorf("hash-object argv = %q, want %q", got, want)
	}
	if !hashObject[0].Spec.Stdin {
		t.Error("hash-object Stdin = false, want the path list on standard input")
	}
	for _, c := range env.Commands() {
		if c.Spec.Argv[1] != "rev-parse" && c.Spec.Argv[1] != "--no-optional-locks" {
			t.Errorf("command %q lacks --no-optional-locks", c.Spec.Argv)
		}
	}

	unchanged, err := target.CompareHandoffEvidenceBaseline(context.Background(), baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline() unchanged error = %v", err)
	}
	if unchanged.WorktreeChanged {
		t.Errorf("unchanged WorktreeChanged = true, want false")
	}

	if err := os.WriteFile(guestFile, []byte("after\n"), 0o600); err != nil {
		t.Fatalf("update untracked file: %v", err)
	}
	changed, err := target.CompareHandoffEvidenceBaseline(context.Background(), baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline() changed error = %v", err)
	}
	if !changed.WorktreeChanged {
		t.Errorf("WorktreeChanged = false, want true after the untracked file changed in the environment")
	}
	if entries, err := os.ReadDir(hostDir); err != nil || len(entries) != 0 {
		t.Errorf("host workspace entries = %v (err %v), want it untouched and empty", entries, err)
	}
}

// TestHandoffEvidence_UnhashableUntrackedPathsKeepTheFingerprint pins
// that untracked paths hash-object refuses (a dangling link, a link to
// a directory) do not fail the inspection, and that a file listed after
// them, under a name that needs quoting on hash-object's input, still
// has its content in the fingerprint.
func TestHandoffEvidence_UnhashableUntrackedPathsKeepTheFingerprint(t *testing.T) {
	t.Parallel()

	dir := newHandoffEvidenceGitWorkspace(t)
	mustSymlink(t, filepath.Join(dir, "missing"), filepath.Join(dir, "a-dangling"))
	if err := os.Mkdir(filepath.Join(dir, "a-dir"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a-dir", "inner.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustSymlink(t, "a-dir", filepath.Join(dir, "b-dirlink"))
	quoted := filepath.Join(dir, "c \"odd\"\\\nname.txt")
	if err := os.WriteFile(quoted, []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write quoted-name file: %v", err)
	}

	baseline, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline() error = %v", err)
	}
	unchanged, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline() unchanged error = %v", err)
	}
	if unchanged.WorktreeChanged {
		t.Errorf("unchanged WorktreeChanged = true, want false")
	}

	if err := os.WriteFile(quoted, []byte("after\n"), 0o600); err != nil {
		t.Fatalf("update quoted-name file: %v", err)
	}
	changed, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline() changed error = %v", err)
	}
	if !changed.WorktreeChanged {
		t.Errorf("WorktreeChanged = false, want true after the quoted-name file changed")
	}
}

// TestHandoffEvidence_TooManyUnhashableUntrackedPathsFails pins the
// bound on hash-object reruns: more unhashable untracked paths than
// the bound fail the inspection instead of running one command each.
func TestHandoffEvidence_TooManyUnhashableUntrackedPathsFails(t *testing.T) {
	t.Parallel()

	dir := newHandoffEvidenceGitWorkspace(t)
	for i := range maxUnhashableUntracked + 1 {
		mustSymlink(t, "missing", filepath.Join(dir, "dangling-"+strconv.Itoa(i)))
	}

	if _, err := CaptureHandoffEvidenceBaseline(context.Background(), dir); err == nil {
		t.Fatal("CaptureHandoffEvidenceBaseline() error = nil, want the unhashable-path bound exceeded")
	}
}
