package orchestrator

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/gitsync"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// SCMMetadataReader reads the SCM metadata of the workspace at a host
// path: the branch, pushed commit, and pull request that CI feedback,
// review reactions, auto-merge, workspace retention, handoff evidence,
// and startup recovery act on. It returns a zero value when the
// workspace has none.
//
// A nil reader reads the agent's .sortie/scm.json, the source of every
// configuration in which the agent pushes its own work.
type SCMMetadataReader func(workspacePath string, logger *slog.Logger) domain.SCMMetadata

// Read reads the SCM metadata of the workspace at workspacePath.
func (r SCMMetadataReader) Read(workspacePath string, logger *slog.Logger) domain.SCMMetadata {
	if r == nil {
		return workspace.ReadSCMMetadata(workspacePath, logger)
	}
	return r(workspacePath, logger)
}

// SCMMetadataSource returns the reader of workspace SCM metadata under
// cfg. When cfg places the repository in an isolated environment, Sortie
// pushes the issue branch itself and the reader returns only the record
// Sortie keeps on the host, ignoring the agent's .sortie/scm.json even
// when Sortie has no record yet: an agent writing that file must not
// point any reaction at another pull request. Otherwise the agent pushes
// and the reader returns its .sortie/scm.json.
func SCMMetadataSource(cfg config.EnvironmentConfig) SCMMetadataReader {
	if configuresRepository(cfg) {
		return workspace.ReadHostSCMRecord
	}
	return workspace.ReadSCMMetadata
}

// scmRecordUpdate is a change to the SCM record Sortie keeps for a
// workspace.
type scmRecordUpdate struct {
	// Push, when non-nil, is an export that pushed the issue branch or
	// found it already upstream. Any other export changes nothing.
	Push *gitsync.ExportResult

	// PRNumber, when positive, is the pull request open for the issue
	// branch.
	PRNumber int

	// Project is the repository's owner/repo path, which names the
	// pull request's repository.
	Project string
}

// updateSCMRecord applies u to the SCM record of the workspace at
// workspacePath, keeping every field u does not change, and writes it
// unless nothing changed. A push of another branch than the record
// names drops the record's pull request, whose head is that other
// branch.
//
// An export that found the branch already upstream records its branch
// and commit, which repairs a record a failed write left behind after
// the push itself succeeded. Its push time is the record's own when the
// record already names that commit, and the current time otherwise: the
// mirror keeps no time of the earlier push, which happened at most one
// run before.
func updateSCMRecord(workspacePath string, u scmRecordUpdate, logger *slog.Logger) error {
	before := workspace.ReadHostSCMRecord(workspacePath, logger)
	rec := before
	if p := u.Push; p != nil && (p.Pushed || p.UpToDate) {
		if rec.Branch != p.Branch {
			rec.PRNumber, rec.Owner, rec.Repo = 0, "", ""
		}
		keepTime := !p.Pushed && rec.Branch == p.Branch && rec.SHA == p.Commit && rec.PushedAt != ""
		rec.Branch = p.Branch
		rec.SHA = p.Commit
		if !keepTime {
			pushedAt := p.PushedAt
			if pushedAt.IsZero() {
				pushedAt = time.Now()
			}
			rec.PushedAt = pushedAt.UTC().Format(time.RFC3339)
		}
	}
	if u.PRNumber > 0 {
		owner, repo, ok := splitProject(u.Project)
		if !ok {
			return fmt.Errorf("record pull request %d: repository project %q is not an owner/repo path", u.PRNumber, u.Project)
		}
		rec.PRNumber, rec.Owner, rec.Repo = u.PRNumber, owner, repo
	}
	if rec == before {
		return nil
	}
	return workspace.WriteHostSCMRecord(workspacePath, rec)
}

// splitProject splits an owner/repo project path at its last slash, so
// a nested owner such as group/subgroup stays whole.
func splitProject(project string) (owner, repo string, ok bool) {
	i := strings.LastIndex(project, "/")
	if i <= 0 || i == len(project)-1 {
		return "", "", false
	}
	return project[:i], project[i+1:], true
}
