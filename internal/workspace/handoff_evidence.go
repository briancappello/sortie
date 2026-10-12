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

	// Untracked files enter the fingerprint as their paths and the blob
	// IDs git computes for them where the commands run, so their content
	// is never read on the host. The paths are relative to the workspace,
	// which is also the directory hash-object resolves them from.
	untrackedArgs := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, pathspecs...)
	untrackedRaw, err := runGit(ctx, run, untrackedArgs...)
	if err != nil {
		return "", zero, fmt.Errorf("list untracked files: %w", err)
	}
	var untracked [][]byte
	for pathBytes := range bytes.SplitSeq(untrackedRaw, []byte{0}) {
		if len(pathBytes) != 0 {
			untracked = append(untracked, pathBytes)
		}
	}
	objectIDs, err := hashUntrackedFiles(ctx, run, untracked)
	if err != nil {
		return "", zero, fmt.Errorf("hash untracked files: %w", err)
	}
	for i, pathBytes := range untracked {
		writeFingerprintPart(hasher, pathBytes)
		writeFingerprintPart(hasher, objectIDs[i])
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

// maxUnhashableUntracked bounds the untracked paths hash-object may
// refuse in one inspection. Each refusal costs another hash-object run
// for the paths after it, so a work tree full of them fails the
// inspection instead of running one command per path.
const maxUnhashableUntracked = 64

// unhashableUntracked stands in for the object ID of an untracked path
// hash-object refuses: a dangling symbolic link, a link to a directory,
// or a nested repository. The path itself still enters the fingerprint.
var unhashableUntracked = []byte("unhashable")

// hashUntrackedFiles returns the blob ID of each path in paths, in
// order, computed by "git hash-object --no-filters --stdin-paths" run
// through target, with the paths on its standard input. hash-object
// stops at the first path it cannot hash, after printing the IDs of
// the paths before it, so that path is recorded as
// [unhashableUntracked] and hash-object runs again from the next one,
// at most [maxUnhashableUntracked] times.
func hashUntrackedFiles(ctx context.Context, target Target, paths [][]byte) ([][]byte, error) {
	ids := make([][]byte, 0, len(paths))
	unhashable := 0
	for len(ids) < len(paths) {
		rest := paths[len(ids):]
		var input bytes.Buffer
		for _, path := range rest {
			input.WriteString(quoteStdinPath(path))
			input.WriteByte('\n')
		}
		output, runErr := runGitInput(ctx, target, input.Bytes(), "hash-object", "--no-filters", "--stdin-paths")
		hashed, parseErr := parseObjectIDs(output)
		if parseErr != nil {
			return nil, parseErr
		}
		if len(hashed) > len(rest) {
			return nil, fmt.Errorf("git hash-object printed %d object IDs for %d paths", len(hashed), len(rest))
		}
		ids = append(ids, hashed...)
		if runErr == nil {
			if len(hashed) != len(rest) {
				return nil, fmt.Errorf("git hash-object printed %d object IDs for %d paths", len(hashed), len(rest))
			}
			break
		}
		var exitErr *exec.ExitError
		if ctx.Err() != nil || !errors.As(runErr, &exitErr) || len(hashed) == len(rest) {
			return nil, runErr
		}
		unhashable++
		if unhashable > maxUnhashableUntracked {
			return nil, fmt.Errorf("more than %d untracked paths cannot be hashed: %w", maxUnhashableUntracked, runErr)
		}
		ids = append(ids, unhashableUntracked)
	}
	return ids, nil
}

// parseObjectIDs splits hash-object's output into its object IDs, one
// per line, each a hexadecimal SHA-1 or SHA-256 name.
func parseObjectIDs(output []byte) ([][]byte, error) {
	var ids [][]byte
	for line := range bytes.SplitSeq(output, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if !isObjectID(line) {
			return nil, fmt.Errorf("git hash-object printed %q, want an object ID", line)
		}
		ids = append(ids, line)
	}
	return ids, nil
}

func isObjectID(b []byte) bool {
	if len(b) != 40 && len(b) != 64 {
		return false
	}
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// quoteStdinPath returns path as one line of hash-object's
// --stdin-paths input: a C-style quoted string, which git unquotes, so
// a path holding a newline, a quote, or a backslash reaches it intact.
func quoteStdinPath(path []byte) string {
	var b strings.Builder
	b.Grow(len(path) + 2)
	b.WriteByte('"')
	for _, c := range path {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			_, _ = fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// GitCommand returns a host git command for args that runs in dir,
// inheriting the host process environment, as [Target.GitCommand] does
// for [HostTarget](dir).
func GitCommand(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	return HostTarget(dir).GitCommand(ctx, nil, args...)
}

// gitInspectionEnv is the environment of the inspection's git commands
// on a host target. Environments that forward only their own variables
// drop it, so [inspectionGitArgs] also passes the equivalent global
// option on the command line.
func gitInspectionEnv() []string {
	return append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
}

// inspectionGitArgs returns args with git's --no-optional-locks global
// option in front, so a command that reads the index never writes a
// refreshed one back while the agent may be using the repository.
// rev-parse reads no index and takes no lock, so its arguments stay as
// they are.
func inspectionGitArgs(args []string) []string {
	if len(args) == 0 || args[0] == "rev-parse" {
		return args
	}
	return append([]string{"--no-optional-locks"}, args...)
}

func runGit(ctx context.Context, target Target, args ...string) ([]byte, error) {
	return runGitInput(ctx, target, nil, args...)
}

// runGitInput runs the inspection git command args through target and
// returns its standard output. A non-nil stdin is the command's
// standard input; nil connects none.
func runGitInput(ctx context.Context, target Target, stdin []byte, args ...string) ([]byte, error) {
	cmd, err := target.gitCommand(ctx, gitInspectionEnv(), stdin != nil, inspectionGitArgs(args)...)
	if err != nil {
		return nil, err
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
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
