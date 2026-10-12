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

// fakeRepository is a [RepositorySync] whose seed writes a marker file
// in the workspace through the environment, so the environment's
// operation order shows where it ran and later hooks can see it.
type fakeRepository struct {
	seedErr error

	mu    sync.Mutex
	seeds []environment.Workspace
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

	// An existing environment is not seeded again.
	if _, err := Prepare(context.Background(), params); err != nil {
		t.Fatalf("second Prepare() error = %v", err)
	}
	if got := len(repo.seeded()); got != 1 {
		t.Errorf("seeds after a second preparation = %d, want 1", got)
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
