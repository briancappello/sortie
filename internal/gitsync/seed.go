package gitsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Seed is a workspace seed built on the host: a bundle file that a
// worker clones into a workspace of its own.
type Seed struct {
	// Branch is the issue branch the clone checks out.
	Branch string

	// Commit is the issue branch's commit in the seed: the upstream
	// issue branch when it exists, otherwise the base branch.
	Commit string
}

// BuildSeed writes the workspace seed of the workspace key to dst, a
// file that must not exist. It fetches the upstream into the mirror,
// failing when that fails, and bundles HEAD and refs/heads/<issue
// branch> only: the upstream issue branch when it exists, otherwise a
// new branch at the base branch. So `git clone` of the bundle gives the
// repository with the issue branch checked out, and origin/<issue
// branch> marks the seed. The bundle is built in a scratch repository
// that borrows the mirror's objects, so no ref of the mirror changes.
func (s *Syncer) BuildSeed(ctx context.Context, key, dst string) (Seed, error) {
	branch := s.IssueBranch(key)
	l, err := s.mirror.Lock(ctx)
	if err != nil {
		return Seed{}, fmt.Errorf("lock the repository mirror: %w", err)
	}
	defer l.Unlock()

	if err := l.CheckBranchName(ctx, branch); err != nil {
		return Seed{}, err
	}
	if err := l.FetchUpstream(ctx); err != nil {
		return Seed{}, err
	}
	commit, ok, err := l.Ref(ctx, branch)
	if err != nil {
		return Seed{}, err
	}
	if !ok {
		if commit, ok, err = l.Ref(ctx, s.repo.Base); err != nil {
			return Seed{}, err
		} else if !ok {
			return Seed{}, fmt.Errorf("base branch %q is not in the mirror of %s", s.repo.Base, s.mirror.displayURL())
		}
	}

	scratch, err := os.MkdirTemp(filepath.Dir(s.mirror.Dir()), "seed-")
	if err != nil {
		return Seed{}, fmt.Errorf("create seed directory: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // a leftover scratch repository only costs disk
	git := runner{binary: l.m.git.binary, gitDir: scratch}
	if _, err := git.run(ctx, "init", "--bare", "--quiet", "--template="); err != nil {
		return Seed{}, fmt.Errorf("create the seed repository: %w", err)
	}
	alternates := filepath.Join(scratch, "objects", "info", "alternates")
	if err := os.WriteFile(alternates, []byte(filepath.Join(s.mirror.Dir(), "objects")+"\n"), 0o600); err != nil {
		return Seed{}, fmt.Errorf("link the seed repository to the mirror: %w", err)
	}
	ref := "refs/heads/" + branch
	for _, args := range [][]string{
		{"update-ref", "--no-deref", ref, commit},
		{"symbolic-ref", "HEAD", ref},
		{"bundle", "create", "--quiet", dst, "HEAD", ref},
	} {
		if _, err := git.run(ctx, args...); err != nil {
			return Seed{}, fmt.Errorf("build the seed of %s: %w", branch, err)
		}
	}
	return Seed{Branch: branch, Commit: commit}, nil
}

// ImportReturned takes the issue branch of the workspace key back from
// the bundle a worker returned at bundlePath, and pushes its new
// commits, as [Syncer.Export] does with a bundle read out of an
// environment. The bundle must name refs/heads/<issue branch> and no
// other ref, and the branch's commit must be seed.Commit or descend
// from it. Any other bundle is refused, naming the ref, and nothing is
// imported or pushed.
func (s *Syncer) ImportReturned(ctx context.Context, key, bundlePath string, seed Seed) (ExportResult, error) {
	branch := s.IssueBranch(key)
	res := ExportResult{Branch: branch}
	if err := s.checkExportRefs(ctx, key, branch); err != nil {
		return res, err
	}
	return s.importAndPush(ctx, key, bundlePath, seed.Commit, res)
}
