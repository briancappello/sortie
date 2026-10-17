//go:build unix

package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireGit skips the test when no git program is on PATH.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// isolateGit points every git command of the test, Sortie's included, at
// globalConfig instead of the developer's configuration, and away from
// the system configuration. An empty globalConfig selects an empty file.
func isolateGit(t *testing.T, globalConfig string) {
	t.Helper()
	if globalConfig == "" {
		globalConfig = filepath.Join(t.TempDir(), "gitconfig")
		if err := os.WriteFile(globalConfig, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// fixtureEnv is the environment of the test's own git commands: an empty
// global configuration, whatever the test set for Sortie's, and a fixed
// identity.
func fixtureEnv(t *testing.T) []string {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "fixture-gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+empty, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
	)
}

// fixtureGit runs a test fixture's git command in dir and returns its
// trimmed standard output.
func fixtureGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: the test's own fixture command
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// upstream is a local repository standing in for the remote one.
type upstream struct {
	t   *testing.T
	env []string
	dir string
}

// newUpstream creates a repository whose main branch holds one commit.
func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{t: t, env: fixtureEnv(t), dir: filepath.Join(t.TempDir(), "upstream")}
	fixtureGit(t, u.env, filepath.Dir(u.dir), "init", "--quiet", "--template=", "-b", "main", u.dir)
	u.commit("README", "hello\n")
	return u
}

func (u *upstream) git(args ...string) string {
	u.t.Helper()
	return fixtureGit(u.t, u.env, u.dir, args...)
}

// commit writes name with content on the current branch, commits it,
// and returns the new commit.
func (u *upstream) commit(name, content string) string {
	u.t.Helper()
	path := filepath.Join(u.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		u.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		u.t.Fatal(err)
	}
	u.git("add", "--", name)
	u.git("commit", "--quiet", "-m", "change "+name)
	return u.git("rev-parse", "HEAD")
}

// mirrorRefs lists every ref of the mirror as "<ref> <commit>" lines.
func mirrorRefs(t *testing.T, env []string, m *Mirror) string {
	t.Helper()
	return fixtureGit(t, env, m.Dir(), "--git-dir="+m.Dir(), "for-each-ref", "--format=%(refname) %(objectname)")
}

func lockMirror(t *testing.T, m *Mirror) *Locked {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := m.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	return l
}

func TestMirrorPath(t *testing.T) {
	t.Parallel()

	got := MirrorPath("/state", "git@example.com:org/app.git")
	dir, base := filepath.Split(got)
	if dir != "/state/repos/" || len(base) != len("0123456789abcdef.git") || !strings.HasSuffix(base, ".git") {
		t.Errorf("MirrorPath() = %q, want /state/repos/<16 hex digits>.git", got)
	}
	if MirrorPath("/state", "git@example.com:org/other.git") == got {
		t.Error("MirrorPath() is the same for two URLs")
	}
	if _, err := OpenMirror("relative/state", "u"); err == nil {
		t.Error("OpenMirror(relative state directory) error = nil, want an error")
	}
	if _, err := OpenMirror("/state", ""); err == nil {
		t.Error("OpenMirror(empty url) error = nil, want an error")
	}
}

func TestFetchUpstream_MirrorsBranchesAndPrunesOnlyThem(t *testing.T) {
	requireGit(t)
	isolateGit(t, "")
	up := newUpstream(t)
	up.git("checkout", "--quiet", "-b", "feature")
	feature := up.commit("feature.txt", "feature\n")
	up.git("checkout", "--quiet", "main")
	main := up.git("rev-parse", "main")
	up.git("tag", "v1")

	m, err := OpenMirror(t.TempDir(), up.dir)
	if err != nil {
		t.Fatal(err)
	}
	l := lockMirror(t, m)
	defer l.Unlock()
	ctx := context.Background()
	if err := l.FetchUpstream(ctx); err != nil {
		t.Fatalf("FetchUpstream() error = %v", err)
	}
	want := fmt.Sprintf("refs/heads/feature %s\nrefs/heads/main %s", feature, main)
	if got := mirrorRefs(t, up.env, m); got != want {
		t.Errorf("mirror refs after the first fetch =\n%s\nwant\n%s", got, want)
	}
	if bare := fixtureGit(t, up.env, m.Dir(), "--git-dir="+m.Dir(), "rev-parse", "--is-bare-repository"); bare != "true" {
		t.Errorf("mirror is-bare-repository = %q, want true", bare)
	}
	if entries, err := os.ReadDir(filepath.Join(m.Dir(), "hooks")); err == nil && len(entries) > 0 {
		t.Errorf("mirror hooks directory holds %d entries, want none from a template", len(entries))
	}

	// A Sortie ref survives a fetch that prunes a deleted branch, and a
	// branch the upstream moved follows it.
	fixtureGit(t, up.env, m.Dir(), "--git-dir="+m.Dir(), "update-ref", "refs/sortie/incoming/KEY-1", feature)
	up.git("branch", "--quiet", "-D", "feature")
	main = up.commit("main.txt", "main\n")
	if err := l.FetchUpstream(ctx); err != nil {
		t.Fatalf("FetchUpstream() second error = %v", err)
	}
	want = fmt.Sprintf("refs/heads/main %s\nrefs/sortie/incoming/KEY-1 %s", main, feature)
	if got := mirrorRefs(t, up.env, m); got != want {
		t.Errorf("mirror refs after the prune =\n%s\nwant\n%s", got, want)
	}

	if commit, ok, err := l.Ref(ctx, "main"); err != nil || !ok || commit != main {
		t.Errorf("Ref(main) = %q, %v, %v; want %q, true, nil", commit, ok, err, main)
	}
	if commit, ok, err := l.Ref(ctx, "feature"); err != nil || ok || commit != "" {
		t.Errorf("Ref(feature) = %q, %v, %v; want absent", commit, ok, err)
	}
}

func TestFetchUpstream_FailureHidesCredentials(t *testing.T) {
	requireGit(t)
	isolateGit(t, "")
	m, err := OpenMirror(t.TempDir(), "https://user:s3cret-token@127.0.0.1:1/org/app.git")
	if err != nil {
		t.Fatal(err)
	}
	l := lockMirror(t, m)
	defer l.Unlock()
	err = l.FetchUpstream(context.Background())
	if err == nil {
		t.Fatal("FetchUpstream(unreachable) error = nil, want an error")
	}
	if strings.Contains(err.Error(), "s3cret-token") {
		t.Errorf("FetchUpstream() error = %q, want the credential left out", err)
	}
	if !strings.Contains(err.Error(), "https://127.0.0.1:1/org/app.git") {
		t.Errorf("FetchUpstream() error = %q, want the URL without credentials", err)
	}
}

func TestCheckBranchName(t *testing.T) {
	requireGit(t)
	isolateGit(t, "")
	m, err := OpenMirror(t.TempDir(), "u")
	if err != nil {
		t.Fatal(err)
	}
	l := lockMirror(t, m)
	defer l.Unlock()
	ctx := context.Background()
	if err := l.CheckBranchName(ctx, "sortie/ENG-42"); err != nil {
		t.Errorf("CheckBranchName(sortie/ENG-42) error = %v, want nil", err)
	}
	for _, bad := range []string{"sortie/ENG..42", "sortie/ENG-42.lock", "-x", "sortie//x"} {
		err := l.CheckBranchName(ctx, bad)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", bad)) {
			t.Errorf("CheckBranchName(%q) error = %v, want one naming the branch", bad, err)
		}
	}
}

func TestLock_SerializesOperations(t *testing.T) {
	requireGit(t)
	isolateGit(t, "")
	state := t.TempDir()
	// Two separate values for one URL, as two workers build them.
	m1, err := OpenMirror(state, "u")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := OpenMirror(state, "u")
	if err != nil {
		t.Fatal(err)
	}

	first := lockMirror(t, m1)
	acquired := make(chan *Locked)
	go func() {
		l, err := m2.Lock(context.Background())
		if err != nil {
			t.Errorf("second Lock() error = %v", err)
			close(acquired)
			return
		}
		acquired <- l
	}()
	select {
	case <-acquired:
		t.Fatal("second Lock() returned while the first holder still held the mirror")
	case <-time.After(200 * time.Millisecond):
	}
	first.Unlock()
	first.Unlock() // a second call does nothing
	select {
	case second := <-acquired:
		if second != nil {
			second.Unlock()
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second Lock() did not return after the first holder unlocked")
	}
}

func TestLock_WaitsForTheLockFileOfAnotherProcess(t *testing.T) {
	requireGit(t)
	isolateGit(t, "")
	m, err := OpenMirror(t.TempDir(), "u")
	if err != nil {
		t.Fatal(err)
	}
	// Create the lock file, then hold it through a descriptor of its own,
	// as another Sortie process would.
	lockMirror(t, m).Unlock()
	other, err := os.OpenFile(m.lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close() //nolint:errcheck // test cleanup

	fd := int(other.Fd()) //nolint:gosec // G115: a small file descriptor
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if l, err := m.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if l != nil {
			l.Unlock()
		}
		t.Fatalf("Lock() while another process holds the file error = %v, want %v", err, context.DeadlineExceeded)
	}

	if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	lockMirror(t, m).Unlock()
}

// evilConfig writes a global git configuration whose filter, hooks, and
// filesystem monitor each create a marker file in markers when they run,
// and returns its path.
func evilConfig(t *testing.T, markers string) string {
	t.Helper()
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"reference-transaction", "post-checkout", "post-merge", "pre-auto-gc"} {
		script := fmt.Sprintf("#!/bin/sh\ntouch '%s/hook-%s'\n", markers, hook)
		if err := os.WriteFile(filepath.Join(hooks, hook), []byte(script), 0o700); err != nil { //nolint:gosec // G306: a hook must be executable
			t.Fatal(err)
		}
	}
	monitor := filepath.Join(t.TempDir(), "fsmonitor")
	if err := os.WriteFile(monitor, fmt.Appendf(nil, "#!/bin/sh\ntouch '%s/fsmonitor'\n", markers), 0o700); err != nil { //nolint:gosec // G306: the monitor must be executable
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`[filter "evil"]
	clean = touch '%[1]s/filter-clean'; cat
	smudge = touch '%[1]s/filter-smudge'; cat
	required = true
[core]
	hooksPath = %[2]s
	fsmonitor = %[3]s
[fetch]
	fsckObjects = false
[transfer]
	fsckObjects = false
`, markers, hooks, monitor)
	path := filepath.Join(t.TempDir(), "evil-gitconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func markerNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFetchBundle_RunsNoFilterOrHookOnTheHost(t *testing.T) {
	requireGit(t)
	markers := t.TempDir()
	evil := evilConfig(t, markers)
	isolateGit(t, evil)

	// The exported branch adds a filter for every path and files shaped
	// like hooks. The fixture commits it without the evil configuration.
	up := newUpstream(t)
	up.commit(".gitattributes", "* filter=evil\n")
	up.commit("hooks/post-checkout", "#!/bin/sh\ntouch /tmp/planted\n")
	tip := up.commit("code.txt", "code\n")
	bundle := filepath.Join(t.TempDir(), "out.bundle")
	up.git("bundle", "create", "--quiet", bundle, "refs/heads/main")
	if got := markerNames(t, markers); len(got) != 0 {
		t.Fatalf("markers after building the fixture = %v, want none", got)
	}

	m, err := OpenMirror(t.TempDir(), up.dir)
	if err != nil {
		t.Fatal(err)
	}
	l := lockMirror(t, m)
	ctx := context.Background()
	if err := l.FetchUpstream(ctx); err != nil {
		t.Fatalf("FetchUpstream() error = %v", err)
	}
	if err := l.FetchBundle(ctx, bundle, "refs/heads/main:refs/sortie/incoming/KEY-1"); err != nil {
		t.Fatalf("FetchBundle() error = %v", err)
	}
	if err := l.CreateBundle(ctx, filepath.Join(t.TempDir(), "in.bundle"), "main"); err != nil {
		t.Fatalf("CreateBundle() error = %v", err)
	}
	l.Unlock()
	if got := fixtureGit(t, up.env, m.Dir(), "--git-dir="+m.Dir(), "rev-parse", "refs/sortie/incoming/KEY-1"); got != tip {
		t.Errorf("refs/sortie/incoming/KEY-1 = %s, want the bundle's tip %s", got, tip)
	}
	if got := markerNames(t, markers); len(got) != 0 {
		t.Errorf("markers after the mirror's fetch, import, and bundle = %v, want none", got)
	}

	// The same configuration does run its filter and hooks for a plain
	// clone, so the check above would have seen them.
	clone := exec.Command("git", "clone", "--quiet", "--branch", "main", bundle, filepath.Join(t.TempDir(), "clone")) //nolint:gosec // G204: the test's own control command
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("control clone: %v: %s", err, out)
	}
	got := strings.Join(markerNames(t, markers), " ")
	if !strings.Contains(got, "filter-smudge") || !strings.Contains(got, "hook-") {
		t.Errorf("markers after a plain clone with the same configuration = %q, want the filter and a hook", got)
	}
}

func TestGitEnv_DropsRepositoryAndCommandLineConfig(t *testing.T) {
	t.Parallel()

	got := gitEnv([]string{
		"HOME=/home/u", "GIT_DIR=/guest/.git", "GIT_WORK_TREE=/guest", "GIT_CONFIG_PARAMETERS='core.hooksPath'='/x'",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/x",
		"GIT_CONFIG_GLOBAL=/home/u/.gitconfig", "SSH_AUTH_SOCK=/run/agent", "GIT_TERMINAL_PROMPT=1",
	})
	want := []string{"HOME=/home/u", "GIT_CONFIG_GLOBAL=/home/u/.gitconfig", "SSH_AUTH_SOCK=/run/agent", "GIT_TERMINAL_PROMPT=0"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("gitEnv() = %q, want %q", got, want)
	}
}
