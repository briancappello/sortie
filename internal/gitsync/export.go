package gitsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/environment"
)

// ExportResult reports what [Syncer.Export] did.
type ExportResult struct {
	// Branch is the issue branch.
	Branch string

	// Commit is the issue branch's commit in the environment; empty
	// when NothingToExport is set.
	Commit string

	// NothingToExport is true when the environment has no issue branch.
	NothingToExport bool

	// Pushed is true when Commit was pushed to the upstream's issue
	// branch.
	Pushed bool

	// PushedAt is when the push completed; zero unless Pushed.
	PushedAt time.Time

	// UpToDate is true when nothing was pushed because Commit already
	// is the tip of the upstream's issue branch, as the mirror last saw
	// it: the work is upstream, as after an earlier push. It is false
	// when the upstream has no issue branch, even if Commit equals the
	// base branch.
	UpToDate bool
}

// IncomingRef returns the mirror ref the issue branch of the workspace
// key is imported into.
func IncomingRef(key string) string { return "refs/sortie/incoming/" + key }

// PushedRef returns the mirror ref that holds the commit Sortie last
// pushed to the upstream's issue branch of the workspace key.
func PushedRef(key string) string { return "refs/sortie/pushed/" + key }

// Export takes the committed issue branch out of ws's environment,
// imports it into the mirror, and pushes it to the upstream when it has
// new commits. Uncommitted changes stay in the environment.
//
// In the environment it bundles refs/heads/<issue branch> under the
// guest bundle directory, then reads the bundle out through a command,
// failing once it exceeds the repository's max_bundle_mib, and removes
// it. An environment without the issue branch has nothing to export:
// Export succeeds with NothingToExport set.
//
// On the host, with the mirror locked, the bundle must name exactly
// refs/heads/<issue branch> and nothing else. Export then fetches the
// upstream into the mirror. When the branch's commit equals the
// upstream's issue branch, or the base branch while the upstream has no
// issue branch, there is nothing to push and Export succeeds without
// importing anything, setting UpToDate in the first case; with the
// upstream unreachable the mirror's last state decides that. Otherwise it imports the branch into
// [IncomingRef], which must then hold a commit, and a failed upstream
// fetch fails the export.
//
// The import is pushed to the upstream's issue branch with a lease: the
// push succeeds only while that branch holds [PushedRef], the commit
// Sortie pushed last, or is absent when Sortie never pushed it. So the
// agent may replace Sortie's own push, as after a rebase, but a commit
// anyone else pushed since is never overwritten: the export fails with
// [ErrBranchMoved] and the upstream keeps that commit. After the push
// Export points [PushedRef] and the mirror's issue branch at the commit.
//
// Every failure before the import leaves the mirror's refs as they were,
// apart from the upstream fetch.
func (s *Syncer) Export(ctx context.Context, env environment.Environment, ws environment.Workspace) (ExportResult, error) {
	branch := s.IssueBranch(ws.Key)
	res := ExportResult{Branch: branch}
	if err := s.checkExportRefs(ctx, ws.Key, branch); err != nil {
		return res, err
	}

	dir, err := os.MkdirTemp(filepath.Dir(s.mirror.Dir()), "bundle-")
	if err != nil {
		return res, fmt.Errorf("create bundle directory: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // a leftover temporary bundle only costs disk
	bundle := filepath.Join(dir, "out.bundle")

	found, err := s.receiveBundle(ctx, env, ws, branch, bundle)
	if err != nil {
		return res, err
	}
	if !found {
		res.NothingToExport = true
		return res, nil
	}

	l, err := s.mirror.Lock(ctx)
	if err != nil {
		return res, fmt.Errorf("lock the repository mirror: %w", err)
	}
	defer l.Unlock()
	commit, needPush, upToDate, err := s.importBundle(ctx, l, ws.Key, branch, bundle)
	res.Commit = commit
	res.UpToDate = upToDate
	if err != nil || !needPush {
		return res, err
	}
	if err := s.push(ctx, l, ws.Key, branch, commit); err != nil {
		return res, err
	}
	res.Pushed = true
	res.PushedAt = time.Now().UTC()
	return res, nil
}

// push pushes the incoming ref of key to the upstream's branch with a
// lease on [PushedRef], then records commit as Sortie's last push and
// as the mirror's copy of the upstream branch.
func (s *Syncer) push(ctx context.Context, l *Locked, key, branch, commit string) error {
	pushed := PushedRef(key)
	expected, _, err := l.RefValue(ctx, pushed)
	if err != nil {
		return err
	}
	if err := l.Push(ctx, IncomingRef(key), branch, expected); err != nil {
		return err
	}
	if err := l.SetRef(ctx, pushed, commit); err != nil {
		return err
	}
	return l.SetRef(ctx, "refs/heads/"+branch, commit)
}

// checkExportRefs fails when branch, or the incoming or pushed ref of
// key, is not a valid name.
func (s *Syncer) checkExportRefs(ctx context.Context, key, branch string) error {
	l, err := s.mirror.Lock(ctx)
	if err != nil {
		return fmt.Errorf("lock the repository mirror: %w", err)
	}
	defer l.Unlock()
	if err := l.CheckBranchName(ctx, branch); err != nil {
		return err
	}
	if err := l.CheckRefName(ctx, IncomingRef(key)); err != nil {
		return err
	}
	return l.CheckRefName(ctx, PushedRef(key))
}

// guestOutBundle returns the exported bundle's path inside the
// environment.
func (s *Syncer) guestOutBundle() string { return path.Join(s.guestDir, "out.bundle") }

// receiveBundle bundles branch in ws's environment and reads the bundle
// into dst on the host. It reports false, having written nothing, when
// the environment has no such branch.
func (s *Syncer) receiveBundle(ctx context.Context, env environment.Environment, ws environment.Workspace, branch, dst string) (bool, error) {
	err := s.guest(ctx, env, ws, nil, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		if procutil.ExtractExitCode(err) == 1 {
			return false, nil
		}
		return false, fmt.Errorf("look up issue branch %q in the environment: %w", branch, err)
	}

	defer s.removeGuestFile(ctx, env, ws, s.guestOutBundle())
	script := `umask 077 && mkdir -p "$1" && rm -f -- "$1/out.bundle" && git bundle create --quiet "$1/out.bundle" "$2"`
	if err := s.guest(ctx, env, ws, nil, "sh", "-c", script, "sh", s.guestDir, "refs/heads/"+branch); err != nil {
		return false, fmt.Errorf("bundle issue branch %q in the environment: %w", branch, err)
	}
	if err := s.readGuestFile(ctx, env, ws, s.guestOutBundle(), dst); err != nil {
		return false, err
	}
	return true, nil
}

// readGuestFile copies the file at guestPath in ws's environment to dst,
// a new host file, through cat run in the environment. It stops the
// command and fails, naming the limit, once the file exceeds
// max_bundle_mib.
func (s *Syncer) readGuestFile(ctx context.Context, env environment.Environment, ws environment.Workspace, guestPath, dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a file in Sortie's own temporary directory
	if err != nil {
		return fmt.Errorf("create the exported bundle file: %w", err)
	}
	defer f.Close() //nolint:errcheck // closed and checked below on success

	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := env.Command(readCtx, ws, environment.CommandSpec{Argv: []string{"cat", "--", guestPath}})
	if err != nil {
		return err
	}
	out := &cappedFile{file: f, limit: int64(s.maxBundleMiB) << 20, stop: cancel}
	stderr := procutil.NewTailBuffer(guestOutputCap)
	res, err := procutil.RunCapture(readCtx, cmd, 0, procutil.CaptureParams{Stdout: out, Stderr: stderr, Logger: s.logger})
	if out.exceeded {
		return fmt.Errorf("the exported bundle exceeds max_bundle_mib (%d MiB)", s.maxBundleMiB)
	}
	if out.err != nil {
		return fmt.Errorf("write the exported bundle file: %w", out.err)
	}
	if err == nil {
		err = res.WaitErr
	}
	if err != nil {
		if msg := strings.TrimSpace(string(stderr.Bytes())); msg != "" {
			return fmt.Errorf("read the exported bundle from the environment: %w: %s", err, msg)
		}
		return fmt.Errorf("read the exported bundle from the environment: %w", err)
	}
	if !res.OutputComplete {
		return errors.New("read the exported bundle from the environment: output incomplete")
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write the exported bundle file: %w", err)
	}
	return nil
}

// cappedFile writes to file until more than limit bytes arrive; then it
// writes nothing more, records that, and calls stop. Every write
// reports success so the capture keeps draining.
type cappedFile struct {
	file     *os.File
	limit    int64
	stop     func()
	n        int64
	exceeded bool
	err      error
}

func (c *cappedFile) Write(p []byte) (int, error) {
	if c.exceeded || c.err != nil {
		return len(p), nil
	}
	if c.n+int64(len(p)) > c.limit {
		c.exceeded = true
		c.stop()
		return len(p), nil
	}
	if _, err := c.file.Write(p); err != nil {
		c.err = err
		c.stop()
		return len(p), nil
	}
	c.n += int64(len(p))
	return len(p), nil
}

// importBundle checks the bundle at bundlePath, fetches the upstream,
// and imports the bundle's branch into the incoming ref of key unless
// there is nothing to push. It returns the branch's commit, whether it
// must be pushed, and whether it is the upstream's issue branch tip.
func (s *Syncer) importBundle(ctx context.Context, l *Locked, key, branch, bundlePath string) (commit string, needPush, upToDate bool, err error) {
	heads, err := l.ListHeads(ctx, bundlePath)
	if err != nil {
		return "", false, false, err
	}
	want := "refs/heads/" + branch
	for _, h := range heads {
		if h.Ref != want {
			return "", false, false, fmt.Errorf("the exported bundle names unexpected ref %q; only %s is accepted", h.Ref, want)
		}
	}
	if len(heads) != 1 {
		return "", false, false, fmt.Errorf("the exported bundle names %s %d times, want once", want, len(heads))
	}
	commit = heads[0].Object
	if !isObjectID(commit) {
		return "", false, false, fmt.Errorf("the exported bundle gives %s the invalid object id %q", want, commit)
	}

	fetchErr := l.FetchUpstream(ctx)
	if fetchErr != nil {
		s.logger.WarnContext(ctx, "upstream fetch failed; comparing the export with the mirror's last state",
			slog.String("repository", s.mirror.displayURL()), slog.Any("error", fetchErr))
	}
	unchanged, upToDate, err := s.matchesUpstream(ctx, l, branch, commit)
	if err != nil {
		return commit, false, false, err
	}
	if unchanged {
		return commit, false, upToDate, nil
	}
	if fetchErr != nil {
		return commit, false, false, fetchErr
	}

	incoming := IncomingRef(key)
	previous, _, err := l.RefValue(ctx, incoming)
	if err != nil {
		return commit, false, false, err
	}
	if err := l.FetchBundle(ctx, bundlePath, "+"+want+":"+incoming); err != nil {
		return commit, false, false, err
	}
	got, ok, err := l.RefValue(ctx, incoming)
	if err == nil && (!ok || got != commit) {
		err = fmt.Errorf("%s holds %q after the import, want %s", incoming, got, commit)
	}
	if err == nil {
		var typ string
		if typ, err = l.ObjectType(ctx, commit); err == nil && typ != "commit" {
			err = fmt.Errorf("the exported %s is a %s, not a commit", want, typ)
		}
	}
	if err != nil {
		if restoreErr := l.SetRef(ctx, incoming, previous); restoreErr != nil {
			return commit, false, false, errors.Join(err, restoreErr)
		}
		return commit, false, false, err
	}
	return commit, true, false, nil
}

// matchesUpstream reports whether commit equals the mirror's issue
// branch, or its base branch when the mirror has no issue branch, and
// whether it equals the issue branch.
func (s *Syncer) matchesUpstream(ctx context.Context, l *Locked, branch, commit string) (unchanged, branchTip bool, err error) {
	tip, ok, err := l.Ref(ctx, branch)
	if err != nil {
		return false, false, err
	}
	if ok {
		return tip == commit, tip == commit, nil
	}
	base, ok, err := l.Ref(ctx, s.repo.Base)
	if err != nil {
		return false, false, err
	}
	return ok && base == commit, false, nil
}
