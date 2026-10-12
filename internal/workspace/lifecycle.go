package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/environment"
)

// logHookLeftovers logs the INFO record naming a hook that exited on
// its own and left a process of its tree behind, when the reap's
// termination reached one.
func logHookLeftovers(ctx context.Context, logger *slog.Logger, hook, workspacePath string, terminatedLeftovers bool) {
	if !terminatedLeftovers {
		return
	}
	logger.InfoContext(ctx, procutil.LeftoversTerminatedMessage, //nolint:sloglint // procutil.LeftoversTerminatedMessage is a fixed string constant
		slog.String("hook", hook), slog.String("workspace", workspacePath))
}

// HookEnv returns the standard SORTIE_* environment variables for
// hook execution. The attempt value is formatted as a decimal string;
// a zero or negative attempt is rendered as "0". When sshHost is
// non-empty, SORTIE_SSH_HOST is included in the returned map.
func HookEnv(issueID, identifier, workspacePath string, attempt int, sshHost ...string) map[string]string {
	if attempt < 0 {
		attempt = 0
	}
	env := map[string]string{
		"SORTIE_ISSUE_ID":         issueID,
		"SORTIE_ISSUE_IDENTIFIER": identifier,
		"SORTIE_WORKSPACE":        workspacePath,
		"SORTIE_ATTEMPT":          strconv.Itoa(attempt),
	}
	if len(sshHost) > 0 && sshHost[0] != "" {
		env["SORTIE_SSH_HOST"] = sshHost[0]
	}
	return env
}

// PrepareParams holds the inputs for workspace preparation before an
// agent run. Construct from orchestrator state at dispatch time.
type PrepareParams struct {
	// Root is the workspace root directory (from config).
	Root string

	// Identifier is the issue identifier used to derive the workspace key.
	Identifier string

	// IssueID is the tracker-assigned issue ID (for hook env vars).
	IssueID string

	// Attempt is the retry attempt number (0 or positive).
	Attempt int

	// AfterCreate is the after_create hook script. Empty means no hook.
	AfterCreate string

	// BeforeRun is the before_run hook script. Empty means no hook.
	BeforeRun string

	// HookTimeoutMS is the timeout for each hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger for hook lifecycle events.
	// If nil, [slog.Default] is used.
	Logger *slog.Logger

	// SSHHost is the SSH destination host for the worker. When non-empty,
	// hooks receive SORTIE_SSH_HOST in their environment.
	SSHHost string

	// PreRunFunc is an optional callback invoked after directory creation
	// (and the after_create hook, if applicable) but before the before_run
	// hook. Receives the absolute workspace path. Nil means no-op.
	PreRunFunc func(workspacePath string)

	// Environment is the execution environment the workspace's hooks run
	// in. Nil runs them as host processes in the workspace directory,
	// which is the local kind.
	Environment environment.Environment

	// EnvironmentForKind builds the environment of a kind other than
	// Environment's, to remove an existing workspace that kind created.
	// Nil handles only a workspace created by the local kind, whose
	// removal is the host directory's alone.
	EnvironmentForKind func(kind string) (environment.Environment, error)
}

// PrepareResult holds the outcome of successful workspace preparation.
type PrepareResult struct {
	// Key is the sanitized workspace directory name.
	Key string

	// Path is the absolute workspace path.
	Path string

	// CreatedNow is true when the workspace directory or its execution
	// environment was newly created, so the after_create hook ran.
	CreatedNow bool
}

// Prepare ensures a workspace directory exists for the given issue,
// prepares its execution environment, and runs the applicable lifecycle
// hooks (after_create if the directory or the environment was newly
// created, then before_run) inside that environment. Returns a
// [PrepareResult] on success.
//
// An existing workspace created by another environment kind is stale:
// the kind recorded in .sortie/environment, or local when there is no
// record, differs from Environment's. Prepare removes that kind's
// instance and the host directory, then prepares the workspace from
// scratch, so after_create runs again. A kind other than local records
// itself, with its instance name when it is an [environment.Namer],
// once its environment is prepared.
//
// Failure semantics:
//   - a stale workspace whose recorded kind cannot be built, or whose
//     instance removal fails, fails preparation and is kept as it is.
//   - an environment preparation failure is fatal: a workspace
//     directory created by this call is removed, and no hook runs, on
//     the host or anywhere else.
//   - after_create failure is fatal: the workspace directory is removed
//     (transactional rollback) so the next retry starts with a clean
//     slate. A failure of the environment's own step after after_create
//     ([environment.AfterCreateFinisher]) is handled the same way.
//   - before_run failure is fatal: returns error. The workspace
//     directory is preserved (it may contain prior agent work).
//
// The context controls cancellation for hook execution. If the context
// is already cancelled when Prepare is called, it returns immediately
// without touching the filesystem.
func Prepare(ctx context.Context, params PrepareParams) (PrepareResult, error) {
	if err := ctx.Err(); err != nil {
		return PrepareResult{}, err
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	wsResult, err := Ensure(params.Root, params.Identifier)
	if err != nil {
		return PrepareResult{}, err
	}
	wsResult, err = recreateStaleWorkspace(ctx, logger, params, wsResult)
	if err != nil {
		return PrepareResult{}, err
	}

	target := NewTarget(params.Environment, wsResult.Key, wsResult.Path, params.IssueID, params.Identifier)
	createdNow := wsResult.CreatedNow
	if params.Environment != nil {
		prep, prepErr := params.Environment.Prepare(ctx, target.Workspace)
		if prepErr != nil {
			logger.WarnContext(ctx, "execution environment preparation failed",
				slog.String("environment", params.Environment.Kind()),
				slog.String("workspace", wsResult.Path), slog.Any("error", prepErr))
			if wsResult.CreatedNow {
				rollbackWorkspace(ctx, logger, wsResult.Path)
			}
			return PrepareResult{}, fmt.Errorf("prepare %s environment: %w", params.Environment.Kind(), prepErr)
		}
		createdNow = createdNow || prep.CreatedNow
	}
	if want, ok := wantedEnvironmentRecord(params.Environment, target.Workspace); ok {
		if err := ensureEnvironmentRecord(wsResult.Path, want); err != nil {
			logger.WarnContext(ctx, "recording the workspace's execution environment failed",
				slog.String("environment", want.Kind),
				slog.String("workspace", wsResult.Path), slog.Any("error", err))
			if wsResult.CreatedNow {
				rollbackWorkspace(ctx, logger, wsResult.Path)
			}
			return PrepareResult{}, fmt.Errorf("record %s environment: %w", want.Kind, err)
		}
	}

	env := HookEnv(params.IssueID, params.Identifier, wsResult.Path, params.Attempt, params.SSHHost)

	if createdNow && params.AfterCreate != "" {
		logger.InfoContext(ctx, "running hook", slog.String("hook", "after_create"), slog.String("workspace", wsResult.Path))
		res, hookErr := RunHook(ctx, HookParams{
			Script:    params.AfterCreate,
			Dir:       wsResult.Path,
			Env:       env,
			TimeoutMS: params.HookTimeoutMS,
			Target:    target,
		})
		if hookErr != nil {
			var he *HookError
			errors.As(hookErr, &he)
			attrs := []slog.Attr{slog.String("hook", "after_create"), slog.String("workspace", wsResult.Path), slog.Any("error", hookErr)}
			if he != nil && he.Output != "" {
				attrs = append(attrs, slog.String("hook_output", he.Output))
			}
			logger.LogAttrs(ctx, slog.LevelWarn, "after_create hook failed, rolling back workspace", attrs...)
			if he != nil {
				logHookLeftovers(ctx, logger, "after_create", wsResult.Path, he.TerminatedLeftovers)
			}
			if rmErr := os.RemoveAll(wsResult.Path); rmErr != nil {
				logger.ErrorContext(ctx, "workspace rollback failed after after_create hook error",
					slog.String("workspace", wsResult.Path), slog.Any("rollback_error", rmErr))
			}
			return PrepareResult{}, hookErr
		}
		logHookLeftovers(ctx, logger, "after_create", wsResult.Path, res.TerminatedLeftovers)
		// A successful hook that produced no output emits no record here;
		// the preceding "running hook" info record already marks that the
		// hook ran.
		if res.Output != "" {
			logger.DebugContext(ctx, "hook completed", slog.String("hook", "after_create"), slog.String("workspace", wsResult.Path), slog.String("hook_output", res.Output))
		}
	}

	if finisher, ok := params.Environment.(environment.AfterCreateFinisher); ok && createdNow {
		if finishErr := finisher.AfterCreate(ctx, target.Workspace); finishErr != nil {
			logger.WarnContext(ctx, "execution environment step after after_create failed, rolling back workspace",
				slog.String("environment", params.Environment.Kind()),
				slog.String("workspace", wsResult.Path), slog.Any("error", finishErr))
			rollbackWorkspace(ctx, logger, wsResult.Path)
			return PrepareResult{}, fmt.Errorf("finish %s environment after after_create: %w", params.Environment.Kind(), finishErr)
		}
	}

	if params.PreRunFunc != nil {
		params.PreRunFunc(wsResult.Path)
	}

	if params.BeforeRun != "" {
		logger.InfoContext(ctx, "running hook", slog.String("hook", "before_run"), slog.String("workspace", wsResult.Path))
		res, hookErr := RunHook(ctx, HookParams{
			Script:    params.BeforeRun,
			Dir:       wsResult.Path,
			Env:       env,
			TimeoutMS: params.HookTimeoutMS,
			Target:    target,
		})
		if hookErr != nil {
			var he *HookError
			errors.As(hookErr, &he)
			attrs := []slog.Attr{slog.String("hook", "before_run"), slog.String("workspace", wsResult.Path), slog.Any("error", hookErr)}
			if he != nil && he.Output != "" {
				attrs = append(attrs, slog.String("hook_output", he.Output))
			}
			logger.LogAttrs(ctx, slog.LevelWarn, "before_run hook failed", attrs...)
			if he != nil {
				logHookLeftovers(ctx, logger, "before_run", wsResult.Path, he.TerminatedLeftovers)
			}
			return PrepareResult{}, hookErr
		}
		logHookLeftovers(ctx, logger, "before_run", wsResult.Path, res.TerminatedLeftovers)
		if res.Output != "" {
			logger.DebugContext(ctx, "hook completed", slog.String("hook", "before_run"), slog.String("workspace", wsResult.Path), slog.String("hook_output", res.Output))
		}
	}

	return PrepareResult{Key: wsResult.Key, Path: wsResult.Path, CreatedNow: createdNow}, nil
}

// rollbackWorkspace removes a workspace directory whose preparation
// failed, so the next attempt starts from a freshly created one.
func rollbackWorkspace(ctx context.Context, logger *slog.Logger, path string) {
	if rmErr := os.RemoveAll(path); rmErr != nil {
		logger.ErrorContext(ctx, "workspace rollback failed after preparation error",
			slog.String("workspace", path), slog.Any("rollback_error", rmErr))
	}
}

// FinishParams holds the inputs for post-agent-run hook execution.
type FinishParams struct {
	// Path is the absolute workspace directory path.
	Path string

	// Identifier is the issue identifier (for hook env vars).
	Identifier string

	// IssueID is the tracker-assigned issue ID (for hook env vars).
	IssueID string

	// Attempt is the retry attempt number.
	Attempt int

	// AfterRun is the after_run hook script. Empty means no hook.
	AfterRun string

	// HookTimeoutMS is the timeout for the hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger. If nil, [slog.Default] is used.
	Logger *slog.Logger

	// SSHHost is the SSH destination host. When non-empty, hooks receive
	// SORTIE_SSH_HOST in their environment.
	SSHHost string

	// SelfReviewStatus is the self-review outcome for hook env.
	// "disabled", "passed", "cap_reached", or "error". Empty treated as "disabled".
	SelfReviewStatus string

	// SelfReviewSummaryPath is the absolute path to .sortie/review_summary.md.
	// Empty when self-review did not run or the summary was not written.
	SelfReviewSummaryPath string

	// Environment is the execution environment the hook runs in. Nil
	// runs it as a host process in the workspace directory.
	Environment environment.Environment
}

// Finish runs the after_run hook if configured. Failure is logged and
// ignored; this function never returns an error.
//
// The parent context is detached via [context.WithoutCancel] so that
// teardown hooks run even when the worker context has been cancelled
// (stall timeout, SIGTERM, reconciliation). Hook execution time is
// still bounded by HookTimeoutMS.
func Finish(ctx context.Context, params FinishParams) {
	if params.AfterRun == "" {
		return
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	detachedCtx := context.WithoutCancel(ctx)
	env := HookEnv(params.IssueID, params.Identifier, params.Path, params.Attempt, params.SSHHost)

	reviewStatus := params.SelfReviewStatus
	if reviewStatus == "" {
		reviewStatus = "disabled"
	}
	env["SORTIE_SELF_REVIEW_STATUS"] = reviewStatus
	if params.SelfReviewSummaryPath != "" {
		env["SORTIE_SELF_REVIEW_SUMMARY_PATH"] = params.SelfReviewSummaryPath
	}

	logger.InfoContext(ctx, "running hook", slog.String("hook", "after_run"), slog.String("workspace", params.Path))
	res, hookErr := RunHook(detachedCtx, HookParams{
		Script:    params.AfterRun,
		Dir:       params.Path,
		Env:       env,
		TimeoutMS: params.HookTimeoutMS,
		Target:    NewTarget(params.Environment, filepath.Base(params.Path), params.Path, params.IssueID, params.Identifier),
	})
	if hookErr != nil {
		var he *HookError
		errors.As(hookErr, &he)
		attrs := []slog.Attr{slog.String("hook", "after_run"), slog.String("workspace", params.Path), slog.Any("error", hookErr)}
		if he != nil && he.Output != "" {
			attrs = append(attrs, slog.String("hook_output", he.Output))
		}
		logger.LogAttrs(ctx, slog.LevelWarn, "after_run hook failed", attrs...)
		if he != nil {
			logHookLeftovers(ctx, logger, "after_run", params.Path, he.TerminatedLeftovers)
		}
	} else {
		logHookLeftovers(ctx, logger, "after_run", params.Path, res.TerminatedLeftovers)
		if res.Output != "" {
			logger.DebugContext(ctx, "hook completed", slog.String("hook", "after_run"), slog.String("workspace", params.Path), slog.String("hook_output", res.Output))
		}
	}
}

// CleanupParams holds the inputs for workspace directory removal.
type CleanupParams struct {
	// Root is the workspace root directory.
	Root string

	// Identifier is the issue identifier.
	Identifier string

	// IssueID is the tracker-assigned issue ID (for hook env vars).
	IssueID string

	// Attempt is the retry attempt number (for hook env vars).
	Attempt int

	// BeforeRemove is the before_remove hook script. Empty means no hook.
	BeforeRemove string

	// HookTimeoutMS is the timeout for the hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger. If nil, [slog.Default] is used.
	Logger *slog.Logger

	// SSHHost is the SSH destination host. When non-empty, hooks receive
	// SORTIE_SSH_HOST in their environment.
	SSHHost string

	// Environment is the execution environment the hook runs in, removed
	// with the workspace. Nil runs the hook as a host process and removes
	// only the directory.
	Environment environment.Environment

	// EnvironmentForKind builds the environment of a kind other than
	// Environment's, to remove a workspace that kind created, as its
	// environment record names it. Nil removes every workspace with
	// Environment.
	EnvironmentForKind func(kind string) (environment.Environment, error)
}

// Cleanup removes a workspace directory for the given issue, running
// the before_remove hook first if configured, then removing the
// workspace's execution environment. Both run in the environment of the
// kind that created the workspace when its record names one other than
// local and EnvironmentForKind is set, and in Environment otherwise. An
// environment whose instance stops between runs ([environment.Resumer])
// is resumed for the hook; without an instance the hook is skipped. The
// before_remove hook failure is logged and ignored; removal still
// proceeds. An environment removal failure is returned and keeps the
// directory.
//
// The parent context is detached via [context.WithoutCancel] so that
// the before_remove hook runs even when the caller's context has been
// cancelled. Hook execution time is still bounded by HookTimeoutMS.
//
// Returns nil if the workspace directory does not exist (idempotent).
// Returns an error if path computation, building the creating kind's
// environment, environment removal, or directory removal fails.
func Cleanup(ctx context.Context, params CleanupParams) error {
	pathResult, err := ComputePath(params.Root, params.Identifier)
	if err != nil {
		return err
	}

	_, statErr := os.Stat(pathResult.Path)
	if os.IsNotExist(statErr) {
		return nil
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	target, err := removalTarget(params.Environment, params.EnvironmentForKind, pathResult.Key, pathResult.Path, params.IssueID, params.Identifier)
	if err != nil {
		return err
	}
	return removeWorkspace(ctx, logger, target, beforeRemove{
		script:    params.BeforeRemove,
		env:       HookEnv(params.IssueID, params.Identifier, pathResult.Path, params.Attempt, params.SSHHost),
		timeoutMS: params.HookTimeoutMS,
	})
}

// beforeRemove is the before_remove hook of one workspace removal.
type beforeRemove struct {
	script    string
	env       map[string]string
	timeoutMS int
}

// removeWorkspace runs the before_remove hook in target's environment,
// best-effort, then removes the environment, then the host directory.
// The hook and the removal run under a context detached from ctx's
// cancellation.
func removeWorkspace(ctx context.Context, logger *slog.Logger, target Target, hook beforeRemove) error {
	detachedCtx := context.WithoutCancel(ctx)
	path := target.Workspace.HostPath

	if hook.script != "" && resumeForHook(ctx, logger, target) {
		logger.InfoContext(ctx, "running hook", slog.String("hook", "before_remove"), slog.String("workspace", path))
		res, hookErr := RunHook(detachedCtx, HookParams{
			Script:    hook.script,
			Dir:       path,
			Env:       hook.env,
			TimeoutMS: hook.timeoutMS,
			Target:    target,
		})
		if hookErr != nil {
			var he *HookError
			errors.As(hookErr, &he)
			attrs := []slog.Attr{slog.String("hook", "before_remove"), slog.String("workspace", path), slog.Any("error", hookErr)}
			if he != nil && he.Output != "" {
				attrs = append(attrs, slog.String("hook_output", he.Output))
			}
			logger.LogAttrs(ctx, slog.LevelWarn, "before_remove hook failed", attrs...)
			if he != nil {
				logHookLeftovers(ctx, logger, "before_remove", path, he.TerminatedLeftovers)
			}
		} else {
			logHookLeftovers(ctx, logger, "before_remove", path, res.TerminatedLeftovers)
			if res.Output != "" {
				logger.DebugContext(ctx, "hook completed", slog.String("hook", "before_remove"), slog.String("workspace", path), slog.String("hook_output", res.Output))
			}
		}
	}

	if err := removeEnvironment(detachedCtx, logger, target); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// resumeForHook makes the target's environment instance ready for the
// before_remove hook when the environment is an [environment.Resumer],
// and reports whether the hook can run. Without an instance, or when
// resuming fails, the hook is skipped and removal proceeds, as after a
// failed hook.
func resumeForHook(ctx context.Context, logger *slog.Logger, target Target) bool {
	resumer, ok := target.Environment.(environment.Resumer)
	if !ok {
		return true
	}
	ready, err := resumer.Resume(context.WithoutCancel(ctx), target.Workspace)
	if err != nil {
		logger.WarnContext(ctx, "before_remove hook skipped: resuming the execution environment failed",
			slog.String("environment", target.Environment.Kind()),
			slog.String("workspace", target.Workspace.HostPath), slog.Any("error", err))
		return false
	}
	if !ready {
		logger.InfoContext(ctx, "before_remove hook skipped: the workspace has no execution environment instance",
			slog.String("environment", target.Environment.Kind()),
			slog.String("workspace", target.Workspace.HostPath))
	}
	return ready
}

// CleanupByPathParams holds the inputs for workspace directory removal
// when the absolute path is already known. Unlike [CleanupParams], this
// does not require a workspace root or identifier-to-path derivation.
type CleanupByPathParams struct {
	// Path is the absolute workspace directory path to remove.
	Path string

	// Identifier is the issue identifier (for hook env vars and logging).
	Identifier string

	// IssueID is the tracker-assigned issue ID (for hook env vars).
	IssueID string

	// Attempt is the retry attempt number (for hook env vars).
	Attempt int

	// BeforeRemove is the before_remove hook script. Empty means no hook.
	BeforeRemove string

	// HookTimeoutMS is the timeout for the hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger. If nil, [slog.Default] is used.
	Logger *slog.Logger

	// SSHHost is the SSH destination host. When non-empty, hooks receive
	// SORTIE_SSH_HOST in their environment.
	SSHHost string

	// Environment is the execution environment the hook runs in, removed
	// with the workspace. Nil runs the hook as a host process and removes
	// only the directory.
	Environment environment.Environment

	// EnvironmentForKind builds the environment of a kind other than
	// Environment's, as [CleanupParams.EnvironmentForKind] describes.
	EnvironmentForKind func(kind string) (environment.Environment, error)
}

// CleanupByPath removes a workspace directory at the given absolute path,
// running the before_remove hook first if configured, then removing the
// workspace's execution environment, both in the environment [Cleanup]
// selects; an environment removal failure is returned and keeps the
// directory. Unlike [Cleanup],
// this function uses the provided path directly instead of reconstructing
// it from workspace root and identifier via [ComputePath].
//
// The caller is responsible for ensuring the path is a valid, contained
// workspace path. This function is intended for cases where the workspace
// path was captured at creation time and the workspace root may have
// changed since then (dynamic config reload).
//
// The parent context is detached via [context.WithoutCancel] so that
// the before_remove hook runs even when the caller's context has been
// cancelled. Hook execution time is still bounded by HookTimeoutMS.
//
// Returns nil if the path does not exist (idempotent).
// Returns an error if the path is empty, not absolute, not in canonical
// form, resolves to the filesystem root, or if environment or directory
// removal fails.
func CleanupByPath(ctx context.Context, params CleanupByPathParams) error {
	if params.Path == "" {
		return errors.New("workspace path must not be empty")
	}
	if !filepath.IsAbs(params.Path) {
		return errors.New("workspace path must be absolute")
	}
	cleaned := filepath.Clean(params.Path)
	if cleaned != params.Path {
		return fmt.Errorf("workspace path must be in canonical form: got %q, cleaned to %q", params.Path, cleaned)
	}
	if cleaned == "/" {
		return errors.New("refusing to remove filesystem root")
	}

	_, statErr := os.Stat(params.Path)
	if os.IsNotExist(statErr) {
		return nil
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	target, err := removalTarget(params.Environment, params.EnvironmentForKind, filepath.Base(params.Path), params.Path, params.IssueID, params.Identifier)
	if err != nil {
		return err
	}
	return removeWorkspace(ctx, logger, target, beforeRemove{
		script:    params.BeforeRemove,
		env:       HookEnv(params.IssueID, params.Identifier, params.Path, params.Attempt, params.SSHHost),
		timeoutMS: params.HookTimeoutMS,
	})
}

// removeEnvironment removes the target's execution environment, after
// the before_remove hook ran in it and before the host directory goes.
// A failure keeps the host directory, so the workspace stays known and a
// later cleanup retries the removal.
func removeEnvironment(ctx context.Context, logger *slog.Logger, target Target) error {
	if target.Environment == nil {
		return nil
	}
	if err := target.Environment.Remove(ctx, target.Workspace); err != nil {
		logger.WarnContext(ctx, "execution environment removal failed, keeping workspace",
			slog.String("environment", target.Environment.Kind()),
			slog.String("workspace", target.Workspace.HostPath), slog.Any("error", err))
		return fmt.Errorf("remove %s environment: %w", target.Environment.Kind(), err)
	}
	return nil
}

// CleanupTerminalParams holds the inputs for batch workspace removal
// of terminal-state issues. Construct from orchestrator state during
// startup cleanup or reconciliation.
type CleanupTerminalParams struct {
	// Root is the workspace root directory (from config).
	Root string

	// Identifiers is the list of issue identifiers whose workspaces
	// should be removed. Each identifier is sanitized to a workspace
	// key before lookup.
	Identifiers []string

	// IssueIDsByIdentifier maps issue identifiers to their
	// tracker-assigned IDs. Used for hook environment variables.
	// Identifiers missing from this map use the identifier as the
	// issue ID fallback.
	IssueIDsByIdentifier map[string]string

	// BeforeRemove is the before_remove hook script. Empty means no hook.
	BeforeRemove string

	// HookTimeoutMS is the timeout for each before_remove hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger for cleanup lifecycle events.
	// If nil, [slog.Default] is used.
	Logger *slog.Logger

	// Environment is the execution environment each workspace's hook
	// runs in, removed with the workspace. Nil runs the hooks as host
	// processes and removes only the directories.
	Environment environment.Environment

	// EnvironmentForKind builds the environment of a kind other than
	// Environment's, as [CleanupParams.EnvironmentForKind] describes.
	EnvironmentForKind func(kind string) (environment.Environment, error)
}

// CleanupTerminalResult holds the outcome of a batch workspace
// cleanup. Inspect Removed for successful removals and Errors for
// per-identifier failures.
type CleanupTerminalResult struct {
	// Removed lists the identifiers whose workspaces were
	// successfully removed or did not exist on disk.
	Removed []string

	// Errors maps identifiers to the error encountered during their
	// cleanup. Identifiers that succeeded or had no workspace on disk
	// are not present in this map.
	Errors map[string]error
}

// CleanupTerminal removes workspace directories for terminal-state
// issues. For each identifier in params.Identifiers, it delegates to
// [Cleanup] which sanitizes the identifier, checks existence, runs the
// before_remove hook (best-effort), and removes the directory.
//
// Cleanup is best-effort per identifier: a failure removing one
// workspace does not prevent cleanup of others. Individual errors are
// collected in [CleanupTerminalResult.Errors].
func CleanupTerminal(ctx context.Context, params CleanupTerminalParams) CleanupTerminalResult {
	cleanup := CleanupTerminalResult{
		Removed: make([]string, 0, len(params.Identifiers)),
		Errors:  make(map[string]error),
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	for _, identifier := range params.Identifiers {
		issueID := identifier
		if params.IssueIDsByIdentifier != nil {
			if mapped, ok := params.IssueIDsByIdentifier[identifier]; ok {
				issueID = mapped
			}
		}

		err := Cleanup(ctx, CleanupParams{
			Root:               params.Root,
			Identifier:         identifier,
			IssueID:            issueID,
			BeforeRemove:       params.BeforeRemove,
			HookTimeoutMS:      params.HookTimeoutMS,
			Logger:             logger,
			Environment:        params.Environment,
			EnvironmentForKind: params.EnvironmentForKind,
		})
		if err != nil {
			logger.WarnContext(ctx, "workspace cleanup failed",
				slog.String("identifier", identifier), slog.Any("error", err))
			cleanup.Errors[identifier] = err
		} else {
			logger.InfoContext(ctx, "workspace cleaned",
				slog.String("identifier", identifier))
			cleanup.Removed = append(cleanup.Removed, identifier)
		}
	}

	return cleanup
}
