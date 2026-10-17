package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// ExportError reports a failed export of a run's issue branch from its
// execution environment. The run fails with it: the tracker is not
// handed off, the environment and its workspace keep the work, and the
// issue is retried with the normal backoff.
type ExportError struct {
	// Branch is the issue branch; empty when the export failed before
	// naming it.
	Branch string

	// Err is the cause.
	Err error
}

func (e *ExportError) Error() string {
	if e.Branch == "" {
		return fmt.Sprintf("export: %v", e.Err)
	}
	return fmt.Sprintf("export issue branch %q: %v", e.Branch, e.Err)
}

func (e *ExportError) Unwrap() error { return e.Err }

// exportParams describes the export of one run's issue branch.
type exportParams struct {
	// Repo is the repository the environment holds.
	Repo Repository

	// Env and Workspace are the run's environment and workspace.
	Env       environment.Environment
	Workspace environment.Workspace

	// Issue is the run's issue.
	Issue domain.Issue

	// PullRequest, when non-nil, is where the issue branch's pull
	// request is opened.
	PullRequest *pullRequestTarget

	Logger *slog.Logger
}

// exportIssueBranch exports the committed issue branch from the
// workspace's environment through the repository and returns the commit
// it pushed, empty when it pushed nothing. A push is recorded in the
// workspace's SCM record. So is a branch found already upstream when the
// record does not name it at that commit, as when recording the push
// that put it there failed. Any other export leaves the record as it is.
//
// With a pull request target, a push ensures the branch's pull request
// and records it; so does a branch found already upstream while the
// record names no pull request, so a pull request a failed call left
// unopened is opened by the next export. A failure, including one to
// record the push or to ensure or record the pull request, is an
// [*ExportError].
func exportIssueBranch(ctx context.Context, p exportParams) (string, error) {
	logger := p.Logger
	ws := p.Workspace
	res, err := p.Repo.Export(ctx, p.Env, ws)
	if err != nil {
		return "", &ExportError{Branch: res.Branch, Err: err}
	}
	var commit string
	switch {
	case res.NothingToExport:
		logger.InfoContext(ctx, "issue branch export skipped, the environment has no issue branch",
			slog.String("branch", res.Branch))
		return "", nil
	case res.Pushed:
		logger.InfoContext(ctx, "issue branch exported and pushed",
			slog.String("branch", res.Branch), slog.String("commit", res.Commit))
		if err := updateSCMRecord(ws.HostPath, scmRecordUpdate{Push: &res}, logger); err != nil {
			return "", &ExportError{Branch: res.Branch, Err: fmt.Errorf("record the push of %s: %w", res.Commit, err)}
		}
		commit = res.Commit
	default:
		logger.InfoContext(ctx, "issue branch export found nothing new to push",
			slog.String("branch", res.Branch), slog.String("commit", res.Commit))
		if !res.UpToDate {
			return "", nil
		}
		if err := updateSCMRecord(ws.HostPath, scmRecordUpdate{Push: &res}, logger); err != nil {
			return "", &ExportError{Branch: res.Branch, Err: fmt.Errorf("record the upstream branch at %s: %w", res.Commit, err)}
		}
		if workspace.ReadHostSCMRecord(ws.HostPath, logger).PRNumber > 0 {
			return "", nil
		}
	}
	if p.PullRequest != nil {
		if err := ensurePullRequest(ctx, *p.PullRequest, ws.HostPath, res.Branch, p.Issue, logger); err != nil {
			return "", &ExportError{Branch: res.Branch, Err: err}
		}
	}
	return commit, nil
}
