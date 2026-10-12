//go:build unix

package smolvm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/gitsync"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// repoFixture is a local upstream repository for the gated repository
// tests, built and inspected by host git with an empty configuration,
// which Sortie's own host git uses too.
type repoFixture struct {
	t   *testing.T
	env []string
	dir string
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := &repoFixture{
		t: t,
		env: append(os.Environ(),
			"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
			"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com"),
		dir: filepath.Join(t.TempDir(), "upstream"),
	}
	f.run(filepath.Dir(f.dir), "init", "--quiet", "--template=", "-b", "main", f.dir)
	f.commit("README", "hello\n")
	return f
}

func (f *repoFixture) run(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: the test's own fixture command
	cmd.Dir = dir
	cmd.Env = f.env
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			stderr = exitErr.Stderr
		}
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}

func (f *repoFixture) git(args ...string) string {
	f.t.Helper()
	return f.run(f.dir, args...)
}

func (f *repoFixture) commit(name, content string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.git("add", "--", name)
	f.git("commit", "--quiet", "-m", "change "+name)
	return f.git("rev-parse", "HEAD")
}

func newSyncer(t *testing.T, url string) *gitsync.Syncer {
	t.Helper()
	syncer, err := gitsync.New(gitsync.Params{
		Repository: config.SmolvmRepository{URL: url, Base: "main", BranchPrefix: "sortie/", MaxBundleMiB: 512},
		StateDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("gitsync.New() error = %v", err)
	}
	return syncer
}

// Not parallel: it sets the git configuration variables in the process
// environment.
func TestIntegration_SeedsTheRepositoryBeforeAfterCreate(t *testing.T) {
	ie := newIntegrationEnv(t)
	up := newRepoFixture(t)
	fresh, existing := ie.newKey("SMOLSEED"), ie.newKey("SMOLSEEDB")
	main := up.git("rev-parse", "main")
	up.git("checkout", "--quiet", "-b", "sortie/"+existing)
	tip := up.commit("work.txt", "an earlier run\n")
	up.git("checkout", "--quiet", "main")
	syncer := newSyncer(t, up.dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prepare := func(key string) {
		t.Helper()
		res, err := workspace.Prepare(ctx, workspace.PrepareParams{
			Root:          ie.root,
			Identifier:    key,
			IssueID:       "10001",
			AfterCreate:   "git rev-parse --abbrev-ref HEAD > /tmp/branch && git rev-parse HEAD origin/main > /tmp/heads",
			HookTimeoutMS: 120_000,
			Environment:   ie.env,
			Repository:    syncer,
		})
		if err != nil {
			t.Fatalf("workspace.Prepare(%s) error = %v", key, err)
		}
		if !res.CreatedNow {
			t.Errorf("workspace.Prepare(%s).CreatedNow = false, want true", key)
		}
	}
	guestFile := func(key, path string) string {
		t.Helper()
		out, err := ie.guest(ctx, key, "cat "+path)
		if err != nil {
			t.Fatalf("guest cat %s: %v", path, err)
		}
		return strings.TrimSpace(out)
	}

	prepare(fresh)
	if got := guestFile(fresh, "/tmp/branch"); got != "sortie/"+fresh {
		t.Errorf("after_create branch = %q, want sortie/%s", got, fresh)
	}
	if got, want := guestFile(fresh, "/tmp/heads"), main+"\n"+main; got != want {
		t.Errorf("after_create HEAD and origin/main = %q, want both at upstream main %s", got, main)
	}
	if out, err := ie.guest(ctx, fresh, "test ! -e /run/sortie/git/in.bundle && readlink /workspace/.sortie && git -C /workspace remote get-url origin"); err != nil ||
		out != GuestControlDir+"\n"+up.dir+"\n" {
		t.Errorf("guest bundle, control link, and origin URL = %q, %v; want no bundle, %s, and %s", out, err, GuestControlDir, up.dir)
	}
	if out, err := ie.guest(ctx, fresh, "cd /workspace && git add -A && git status --porcelain --untracked-files=all"); err != nil || out != "" {
		t.Errorf("guest status after add -A with the control link = %q, %v; want it clean", out, err)
	}

	prepare(existing)
	if got := guestFile(existing, "/tmp/branch"); got != "sortie/"+existing {
		t.Errorf("after_create branch = %q, want sortie/%s", got, existing)
	}
	if got := guestFile(existing, "/tmp/heads"); got != tip+"\n"+main {
		t.Errorf("after_create HEAD and origin/main = %q, want the upstream branch tip %s and main %s", got, tip, main)
	}
}

// Not parallel: it sets the git configuration variables in the process
// environment.
func TestIntegration_RefreshesOriginBeforeTheNextRun(t *testing.T) {
	ie := newIntegrationEnv(t)
	up := newRepoFixture(t)
	key := ie.newKey("SMOLREFRESH")
	syncer := newSyncer(t, up.dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prepare := func(wantCreated bool) {
		t.Helper()
		res, err := workspace.Prepare(ctx, workspace.PrepareParams{
			Root:          ie.root,
			Identifier:    key,
			IssueID:       "10001",
			HookTimeoutMS: 120_000,
			Environment:   ie.env,
			Repository:    syncer,
		})
		if err != nil {
			t.Fatalf("workspace.Prepare() error = %v", err)
		}
		if res.CreatedNow != wantCreated {
			t.Fatalf("workspace.Prepare().CreatedNow = %v, want %v", res.CreatedNow, wantCreated)
		}
	}

	prepare(true)
	// The first run commits locally and leaves a file uncommitted.
	head, err := ie.guest(ctx, key, `cd /workspace && echo work > agent.txt && git add agent.txt &&
git -c user.name=Agent -c user.email=agent@example.com commit -q -m work &&
echo draft > draft.txt && git rev-parse HEAD`)
	if err != nil {
		t.Fatalf("guest local work: %v", err)
	}
	if err := ie.env.cli.Stop(ctx, VMName(key)); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	newMain := up.commit("main.txt", "moved on\n")

	prepare(false)
	out, err := ie.guest(ctx, key, "cd /workspace && git rev-parse origin/main HEAD && git rev-parse --abbrev-ref HEAD && cat draft.txt && git status --porcelain -- agent.txt draft.txt")
	if err != nil {
		t.Fatalf("guest inspection: %v", err)
	}
	want := newMain + "\n" + head + "sortie/" + key + "\ndraft\n?? draft.txt\n"
	if out != want {
		t.Errorf("guest origin/main, HEAD, branch, draft.txt, and status =\n%s\nwant\n%s", out, want)
	}
}
