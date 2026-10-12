//go:build unix

package workspace

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
)

// fakeRepository is a [RepositorySync] whose seed and refresh each write
// a marker file in the workspace through the environment, so the
// environment's operation order shows where they ran and later hooks can
// see them.
type fakeRepository struct {
	seedErr    error
	refreshErr error

	mu        sync.Mutex
	seeds     []environment.Workspace
	refreshes []environment.Workspace
}

func (f *fakeRepository) Seed(ctx context.Context, env environment.Environment, ws environment.Workspace) error {
	f.mu.Lock()
	f.seeds = append(f.seeds, ws)
	f.mu.Unlock()
	if f.seedErr != nil {
		return f.seedErr
	}
	cmd, err := env.Command(ctx, ws, environment.CommandSpec{Argv: []string{"sh", "-c", "echo seeded > seeded"}})
	if err != nil {
		return err
	}
	return cmd.Run()
}

func (f *fakeRepository) Refresh(ctx context.Context, env environment.Environment, ws environment.Workspace) error {
	f.mu.Lock()
	f.refreshes = append(f.refreshes, ws)
	f.mu.Unlock()
	if f.refreshErr != nil {
		return f.refreshErr
	}
	cmd, err := env.Command(ctx, ws, environment.CommandSpec{Argv: []string{"sh", "-c", "echo refreshed > refreshed"}})
	if err != nil {
		return err
	}
	return cmd.Run()
}

func (f *fakeRepository) refreshed() []environment.Workspace {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refreshes)
}

func (f *fakeRepository) seeded() []environment.Workspace {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seeds)
}

func TestPrepare_SeedsRepositoryBeforeAfterCreate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	env := recordingEnvironment()
	repo := &fakeRepository{}
	params := PrepareParams{
		Root:          root,
		Identifier:    "REPO-1",
		IssueID:       "id-repo-1",
		AfterCreate:   "test -f seeded",
		BeforeRun:     "true",
		HookTimeoutMS: 5000,
		Environment:   env,
		Repository:    repo,
	}
	res, err := Prepare(context.Background(), params)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	wantOps := []environmenttest.Operation{
		environmenttest.OpPrepare,
		environmenttest.OpCommand, // seed
		environmenttest.OpCommand, // after_create
		environmenttest.OpAfterCreate,
		environmenttest.OpCommand, // before_run
	}
	if got := env.Ops(); !slices.Equal(got, wantOps) {
		t.Fatalf("environment ops = %q, want %q", got, wantOps)
	}
	wantWS := environment.Workspace{Key: "REPO-1", HostPath: res.Path, GuestPath: res.Path, IssueID: "id-repo-1", Identifier: "REPO-1"}
	if got := repo.seeded(); !slices.Equal(got, []environment.Workspace{wantWS}) {
		t.Errorf("seeded workspaces = %+v, want [%+v]", got, wantWS)
	}
	if got := repo.refreshed(); len(got) != 0 {
		t.Errorf("refreshed workspaces = %+v, want none for a new environment", got)
	}
}

func TestPrepare_RefreshesRepositoryBeforeBeforeRun(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	params := PrepareParams{
		Root:          root,
		Identifier:    "REPO-4",
		IssueID:       "id-repo-4",
		AfterCreate:   "touch created",
		BeforeRun:     "true",
		HookTimeoutMS: 5000,
		Environment:   recordingEnvironment(),
		Repository:    &fakeRepository{},
	}
	if _, err := Prepare(context.Background(), params); err != nil {
		t.Fatalf("first Prepare() error = %v", err)
	}

	// The next run finds the environment the first one created.
	env := recordingEnvironment()
	repo := &fakeRepository{}
	params.Environment, params.Repository = env, repo
	params.BeforeRun = "test -f refreshed"
	if err := os.Remove(root + "/REPO-4/seeded"); err != nil {
		t.Fatal(err)
	}
	res, err := Prepare(context.Background(), params)
	if err != nil {
		t.Fatalf("second Prepare() error = %v", err)
	}

	wantOps := []environmenttest.Operation{
		environmenttest.OpPrepare,
		environmenttest.OpCommand, // refresh
		environmenttest.OpCommand, // before_run
	}
	if got := env.Ops(); !slices.Equal(got, wantOps) {
		t.Fatalf("environment ops = %q, want %q", got, wantOps)
	}
	wantWS := environment.Workspace{Key: "REPO-4", HostPath: res.Path, GuestPath: res.Path, IssueID: "id-repo-4", Identifier: "REPO-4"}
	if got := repo.refreshed(); !slices.Equal(got, []environment.Workspace{wantWS}) {
		t.Errorf("refreshed workspaces = %+v, want [%+v]", got, wantWS)
	}
	if got := repo.seeded(); len(got) != 0 {
		t.Errorf("seeded workspaces = %+v, want none for an existing environment", got)
	}
}

func TestPrepare_RefreshFailureKeepsWorkspaceAndRunsNoHook(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	params := PrepareParams{
		Root:          root,
		Identifier:    "REPO-5",
		HookTimeoutMS: 5000,
		Environment:   recordingEnvironment(),
		Repository:    &fakeRepository{},
	}
	if _, err := Prepare(context.Background(), params); err != nil {
		t.Fatalf("first Prepare() error = %v", err)
	}
	if err := os.WriteFile(root+"/REPO-5/work", []byte("earlier run"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := recordingEnvironment()
	params.Environment = env
	params.Repository = &fakeRepository{refreshErr: errors.New("guest fetch failed")}
	params.BeforeRun = "touch ran"
	_, err := Prepare(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "refresh repository: guest fetch failed") {
		t.Fatalf("Prepare() error = %v, want the refresh failure", err)
	}
	if got, want := env.Ops(), []environmenttest.Operation{environmenttest.OpPrepare}; !slices.Equal(got, want) {
		t.Errorf("environment ops = %q, want %q: before_run does not run after a failed refresh", got, want)
	}
	if _, statErr := os.Stat(root + "/REPO-5/work"); statErr != nil {
		t.Errorf("workspace file after a failed refresh: %v, want the workspace kept", statErr)
	}
}

func TestPrepare_SeedFailureRollsBackAndRunsNoHook(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	env := recordingEnvironment()
	_, err := Prepare(context.Background(), PrepareParams{
		Root:          root,
		Identifier:    "REPO-2",
		AfterCreate:   "touch created",
		BeforeRun:     "touch ran",
		HookTimeoutMS: 5000,
		Environment:   env,
		Repository:    &fakeRepository{seedErr: errors.New("upstream unreachable")},
	})
	if err == nil || !strings.Contains(err.Error(), "seed repository: upstream unreachable") {
		t.Fatalf("Prepare() error = %v, want the seed failure", err)
	}
	if got, want := env.Ops(), []environmenttest.Operation{environmenttest.OpPrepare}; !slices.Equal(got, want) {
		t.Errorf("environment ops = %q, want %q: no hook runs after a failed seed", got, want)
	}
	if _, statErr := os.Stat(root + "/REPO-2"); !os.IsNotExist(statErr) {
		t.Errorf("workspace directory after a failed seed: Stat error = %v, want it removed", statErr)
	}
}

func TestPrepare_RepositoryWithoutEnvironmentFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repo := &fakeRepository{}
	_, err := Prepare(context.Background(), PrepareParams{
		Root:       root,
		Identifier: "REPO-3",
		Repository: repo,
	})
	if err == nil {
		t.Fatal("Prepare(repository, no environment) error = nil, want an error")
	}
	if _, statErr := os.Stat(root + "/REPO-3"); !os.IsNotExist(statErr) {
		t.Errorf("workspace directory: Stat error = %v, want none created", statErr)
	}
	if got := repo.seeded(); len(got) != 0 {
		t.Errorf("seeded workspaces = %+v, want none", got)
	}
}
