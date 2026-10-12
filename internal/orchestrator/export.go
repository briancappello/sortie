package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/environment"
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

// exportIssueBranch exports the committed issue branch from ws's
// environment through repo and returns the commit it pushed, empty when
// it pushed nothing. A failure is an [*ExportError].
func exportIssueBranch(ctx context.Context, repo Repository, env environment.Environment, ws environment.Workspace, logger *slog.Logger) (string, error) {
	res, err := repo.Export(ctx, env, ws)
	if err != nil {
		return "", &ExportError{Branch: res.Branch, Err: err}
	}
	switch {
	case res.NothingToExport:
		logger.InfoContext(ctx, "issue branch export skipped, the environment has no issue branch",
			slog.String("branch", res.Branch))
	case res.Pushed:
		logger.InfoContext(ctx, "issue branch exported and pushed",
			slog.String("branch", res.Branch), slog.String("commit", res.Commit))
		return res.Commit, nil
	default:
		logger.InfoContext(ctx, "issue branch export found nothing new to push",
			slog.String("branch", res.Branch), slog.String("commit", res.Commit))
	}
	return "", nil
}
