package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// repositoryRecordFile names the host record of the repository a
// workspace was seeded from.
const repositoryRecordFile = "repository.json"

const repositoryRecordMaxBytes = 4096

// RepositoryRecord names the configured repository a workspace was
// seeded from.
type RepositoryRecord struct {
	// Name is the repository's configured name, empty for an unnamed
	// single repository block.
	Name string `json:"name"`
}

// ReadRepositoryRecord returns the repository record of the workspace at
// workspacePath. ok is false, with a nil error, when the workspace or
// its record does not exist.
func ReadRepositoryRecord(workspacePath string) (rec RepositoryRecord, ok bool, err error) {
	data, err := workspacekit.ReadHostFile(workspacePath, repositoryRecordFile, repositoryRecordMaxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return RepositoryRecord{}, false, nil
	}
	if err != nil {
		return RepositoryRecord{}, false, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return RepositoryRecord{}, false, fmt.Errorf("%s/%s: %w", workspacekit.HostDir, repositoryRecordFile, err)
	}
	return rec, true, nil
}

// WriteRepositoryRecord records rec as the repository the workspace at
// workspacePath was seeded from.
func WriteRepositoryRecord(workspacePath string, rec RepositoryRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := workspacekit.WriteHostFile(workspacePath, repositoryRecordFile, data); err != nil {
		return fmt.Errorf("write %s/%s: %w", workspacekit.HostDir, repositoryRecordFile, err)
	}
	return nil
}
