package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// scmMetadataMaxBytes is the maximum number of bytes read from the SCM
// metadata file. Legitimate metadata is under 500 bytes; 4 KiB bounds
// memory usage while providing headroom.
const scmMetadataMaxBytes = 4096

// scmMetadataFile is the SCM metadata file name, the same in .sortie and
// in .sortie-host.
const scmMetadataFile = "scm.json"

// ReadSCMMetadata reads the workspace SCM metadata from
// <workspacePath>/.sortie/scm.json.
//
// Returns a zero-value [domain.SCMMetadata] when the file is absent,
// unreadable, oversized, malformed, or when the workspace directory,
// .sortie, or scm.json is a symbolic link or otherwise refused. Read
// errors, other than absence, are logged at warn level; the function
// never returns an error to the caller. Branch may be empty in the
// returned value: some reaction kinds require no checkout and
// therefore no branch, so validating Branch is the caller's
// responsibility, not this reader's.
func ReadSCMMetadata(workspacePath string, logger *slog.Logger) domain.SCMMetadata {
	if logger == nil {
		logger = slog.Default()
	}
	meta, readErr, parseErr := decodeSCMMetadata(workspacekit.ReadSortieFile(workspacePath, scmMetadataFile, scmMetadataMaxBytes))
	switch {
	case readErr != nil:
		logger.Warn("failed to read .sortie/scm.json", slog.String("workspace", workspacePath), slog.Any("error", readErr))
	case parseErr != nil:
		logger.Warn("malformed .sortie/scm.json", slog.String("workspace", workspacePath), slog.Any("error", parseErr))
	}
	return meta
}

// ReadHostSCMRecord reads the SCM record Sortie itself keeps for the
// workspace at <workspacePath>/.sortie-host/scm.json, a directory no
// execution environment mounts, so the agent cannot write it. The record
// has the shape of [ReadSCMMetadata]'s file and is read with the same
// refusals and size limit; it returns a zero-value [domain.SCMMetadata]
// in the same cases, never falling back to the agent's file.
func ReadHostSCMRecord(workspacePath string, logger *slog.Logger) domain.SCMMetadata {
	if logger == nil {
		logger = slog.Default()
	}
	meta, readErr, parseErr := decodeSCMMetadata(workspacekit.ReadHostFile(workspacePath, scmMetadataFile, scmMetadataMaxBytes))
	switch {
	case readErr != nil:
		logger.Warn("failed to read .sortie-host/scm.json", slog.String("workspace", workspacePath), slog.Any("error", readErr))
	case parseErr != nil:
		logger.Warn("malformed .sortie-host/scm.json", slog.String("workspace", workspacePath), slog.Any("error", parseErr))
	}
	return meta
}

// WriteHostSCMRecord replaces the workspace's SCM record read by
// [ReadHostSCMRecord] with rec, atomically, creating the private
// .sortie-host directory when it is absent.
func WriteHostSCMRecord(workspacePath string, rec domain.SCMMetadata) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(data) > scmMetadataMaxBytes {
		return fmt.Errorf("%s/%s: %w", workspacekit.HostDir, scmMetadataFile, workspacekit.ErrTooLarge)
	}
	if err := workspacekit.WriteHostFile(workspacePath, scmMetadataFile, data); err != nil {
		return fmt.Errorf("write %s/%s: %w", workspacekit.HostDir, scmMetadataFile, err)
	}
	return nil
}

// decodeSCMMetadata decodes SCM metadata data read with err. It returns
// a zero value with readErr set for a read failure other than absence,
// and with parseErr set for content that does not decode.
func decodeSCMMetadata(data []byte, err error) (meta domain.SCMMetadata, readErr, parseErr error) {
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return domain.SCMMetadata{}, nil, nil
		}
		return domain.SCMMetadata{}, err, nil
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return domain.SCMMetadata{}, nil, err
	}
	return meta, nil, nil
}
