package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// ErrNotGitWorkspace identifies a workspace that cannot provide a Git
// baseline. Callers treat this as an undeterminable evidence verdict rather
// than as a worker failure.
var ErrNotGitWorkspace = errors.New("workspace is not a Git work tree")

// HandoffEvidenceBaseline captures the durable Git state immediately before
// the agent session starts. The working-tree fingerprint excludes Sortie's
// .sortie control directory; pushed commit and pull-request metadata from that
// directory is evaluated separately as a positive signal.
type HandoffEvidenceBaseline struct {
	Commit              string
	WorktreeFingerprint [sha256.Size]byte
}

// HandoffEvidenceChange describes the workspace differences that can
// independently establish work observed for a run.
type HandoffEvidenceChange struct {
	CommitMoved     bool
	WorktreeChanged bool
}

// CaptureHandoffEvidenceBaseline inspects the Git workspace at
// workspacePath with host git, as [Target.CaptureHandoffEvidenceBaseline]
// does for [HostTarget](workspacePath).
func CaptureHandoffEvidenceBaseline(ctx context.Context, workspacePath string) (HandoffEvidenceBaseline, error) {
	return HostTarget(workspacePath).CaptureHandoffEvidenceBaseline(ctx)
}

// CompareHandoffEvidenceBaseline compares the Git workspace at
// workspacePath with baseline using host git, as
// [Target.CompareHandoffEvidenceBaseline] does for
// [HostTarget](workspacePath).
func CompareHandoffEvidenceBaseline(ctx context.Context, workspacePath string, baseline HandoffEvidenceBaseline) (HandoffEvidenceChange, error) {
	return HostTarget(workspacePath).CompareHandoffEvidenceBaseline(ctx, baseline)
}

// CaptureHandoffEvidenceBaseline inspects the target's Git workspace
// without modifying its index or working tree. Every git command runs
// through [Target.Command].
func (t Target) CaptureHandoffEvidenceBaseline(ctx context.Context) (HandoffEvidenceBaseline, error) {
	commit, fingerprint, err := inspectHandoffEvidenceState(ctx, t)
	if err != nil {
		return HandoffEvidenceBaseline{}, err
	}
	return HandoffEvidenceBaseline{
		Commit:              commit,
		WorktreeFingerprint: fingerprint,
	}, nil
}

// CompareHandoffEvidenceBaseline compares the target's current Git state
// with the run's frozen baseline. Every git command runs through
// [Target.Command].
func (t Target) CompareHandoffEvidenceBaseline(ctx context.Context, baseline HandoffEvidenceBaseline) (HandoffEvidenceChange, error) {
	commit, fingerprint, err := inspectHandoffEvidenceState(ctx, t)
	if err != nil {
		return HandoffEvidenceChange{}, err
	}
	return HandoffEvidenceChange{
		CommitMoved:     commit != baseline.Commit,
		WorktreeChanged: fingerprint != baseline.WorktreeFingerprint,
	}, nil
}

func inspectHandoffEvidenceState(ctx context.Context, target Target) (string, [sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	workspacePath := target.Workspace.HostPath
	if workspacePath == "" {
		return "", zero, fmt.Errorf("workspace path is empty")
	}

	absPath, err := filepath.Abs(workspacePath)
	if err != nil {
		return "", zero, fmt.Errorf("resolve workspace path: %w", err)
	}
	if err := workspacekit.VerifyDir(absPath); err != nil {
		return "", zero, fmt.Errorf("verify workspace directory: %w", err)
	}
	absPath, err = filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", zero, fmt.Errorf("resolve workspace symlinks: %w", err)
	}

	// A host target runs git in the resolved directory. An environment
	// runs it in its own workspace, and the work-tree root git reports is
	// a path inside that environment, so the containment check compares
	// it with the environment's path for the resolved directory.
	run := target
	if run.Environment == nil {
		run = HostTarget(absPath)
	}
	workspaceGitPath := run.GuestPath(absPath)

	inside, err := runGit(ctx, run, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		return "", zero, ErrNotGitWorkspace
	}

	topRaw, err := runGit(ctx, run, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", zero, fmt.Errorf("resolve Git work-tree root: %w", err)
	}
	top := filepath.Clean(strings.TrimSpace(string(topRaw)))
	relWorkspace, err := filepath.Rel(top, workspaceGitPath)
	if err != nil || relWorkspace == ".." || strings.HasPrefix(relWorkspace, ".."+string(filepath.Separator)) {
		return "", zero, fmt.Errorf("workspace %q is outside Git work-tree root %q", workspaceGitPath, top)
	}

	commitRaw, commitErr := runGit(ctx, run, "rev-parse", "--verify", "--quiet", "HEAD")
	commit := strings.TrimSpace(string(commitRaw))
	if commitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(commitErr, &exitErr) || exitErr.ExitCode() != 1 {
			return "", zero, fmt.Errorf("inspect Git HEAD: %w", commitErr)
		}
		commit = ""
	}

	pathspecs := handoffEvidencePathspecs(relWorkspace)
	hasher := sha256.New()
	for _, command := range [][]string{
		{"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=no", "--"},
		{"diff", "--binary", "--no-ext-diff", "--no-textconv", "--submodule=diff", "--"},
		{"diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--submodule=diff", "--"},
	} {
		args := append(command, pathspecs...)
		output, cmdErr := runGit(ctx, run, args...)
		if cmdErr != nil {
			return "", zero, fmt.Errorf("inspect Git working tree: %w", cmdErr)
		}
		writeFingerprintPart(hasher, output)
	}

	untrackedArgs := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--full-name", "--"}, pathspecs...)
	untrackedRaw, err := runGit(ctx, run, untrackedArgs...)
	if err != nil {
		return "", zero, fmt.Errorf("list untracked files: %w", err)
	}
	for pathBytes := range bytes.SplitSeq(untrackedRaw, []byte{0}) {
		if len(pathBytes) == 0 {
			continue
		}
		relPath := string(pathBytes)
		writeFingerprintPart(hasher, pathBytes)
		if err := hashUntrackedFile(hasher, filepath.Join(top, filepath.FromSlash(relPath))); err != nil {
			return "", zero, fmt.Errorf("inspect untracked file %q: %w", relPath, err)
		}
	}

	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hasher.Sum(nil))
	return commit, fingerprint, nil
}

func handoffEvidencePathspecs(relWorkspace string) []string {
	relWorkspace = filepath.ToSlash(filepath.Clean(relWorkspace))
	dotSortie := workspacekit.SortieDir
	if relWorkspace != "." {
		dotSortie = relWorkspace + "/" + workspacekit.SortieDir
	}
	return []string{
		".",
		":(top,exclude)" + dotSortie,
		":(top,exclude)" + dotSortie + "/**",
	}
}

func writeFingerprintPart(dst io.Writer, value []byte) {
	_, _ = fmt.Fprintf(dst, "%d:", len(value))
	_, _ = dst.Write(value)
}

func hashUntrackedFile(dst io.Writer, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	writeFingerprintPart(dst, []byte(info.Mode().String()))

	switch {
	case info.Mode().IsRegular():
		file, err := os.Open(path) //nolint:gosec // path comes from git ls-files under the inspected work tree
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		_, _ = fmt.Fprintf(dst, "%d:", info.Size())
		if _, err := io.Copy(dst, file); err != nil {
			return err
		}
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		writeFingerprintPart(dst, []byte(target))
		return nil
	default:
		return fmt.Errorf("unsupported file mode %s", info.Mode())
	}
}

// GitCommand returns a host git command for args that runs in dir,
// inheriting the host process environment, as [Target.GitCommand] does
// for [HostTarget](dir).
func GitCommand(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	return HostTarget(dir).GitCommand(ctx, nil, args...)
}

func runGit(ctx context.Context, target Target, args ...string) ([]byte, error) {
	cmd, err := target.GitCommand(ctx, append(os.Environ(), "GIT_OPTIONAL_LOCKS=0"), args...)
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	result, startErr := procutil.RunCapture(ctx, cmd, procutil.DefaultStopGrace, procutil.CaptureParams{Stdout: &stdout, Stderr: &stderr})

	reportErr := startErr
	if reportErr == nil {
		reportErr = result.WaitErr
	}
	if reportErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return stdout.Bytes(), fmt.Errorf("git %s: %s: %w", args[0], message, reportErr)
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w", args[0], reportErr)
	}
	if !result.OutputComplete {
		return stdout.Bytes(), fmt.Errorf("git %s: output did not complete within %s", args[0], procutil.DefaultDrainGrace)
	}
	return stdout.Bytes(), nil
}
