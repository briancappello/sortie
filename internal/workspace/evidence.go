package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"regexp"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// EvidenceFileName is the file, in the .sortie directory, in which an
// agent that declares no-change-needed cites where the work was done.
const EvidenceFileName = "evidence.json"

// Limits of the evidence file.
const (
	evidenceFileMaxBytes = 4 << 10
	evidenceNoteMaxRunes = 2000
)

// Evidence citation kinds.
const (
	EvidenceCommit  = "commit"
	EvidencePlanKey = "plan_key"
)

// Why a claim has no usable citation.
const (
	EvidenceMissing    = "no_evidence"
	EvidenceUnparsed   = "evidence_unparsed"
	EvidenceNoCitation = "no_citation"
)

var (
	fullSHA         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	evidencePlanKey = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*(/[a-z0-9]+(-[a-z0-9]+)*)?$`)
)

// Evidence is a parsed citation. It is untrusted input from the agent:
// Sortie confirms it on the host before it counts.
type Evidence struct {
	Kind    string `json:"kind"`
	SHA     string `json:"sha,omitempty"`
	PlanKey string `json:"plan_key,omitempty"`
	Note    string `json:"note,omitempty"`
}

// ReadEvidenceFile reads <workspacePath>/.sortie/evidence.json through
// the same refusals as the status file: no links, a plain file, at most
// 4 KiB. It returns the evidence, or a reason it has none.
func ReadEvidenceFile(workspacePath string, logger *slog.Logger) (Evidence, string) {
	if logger == nil {
		logger = slog.Default()
	}
	data, err := workspacekit.ReadSortieFile(workspacePath, EvidenceFileName, evidenceFileMaxBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Evidence{}, EvidenceMissing
	case err != nil:
		logger.Warn("refused .sortie/evidence.json", slog.String("workspace", workspacePath), slog.Any("error", err))
		return Evidence{}, EvidenceUnparsed
	}
	return ParseEvidence(data)
}

// ParseEvidence parses an evidence document. Exactly one citation is
// accepted: a full 40-hex commit, or a plan key.
func ParseEvidence(data []byte) (Evidence, string) {
	var ev Evidence
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil || dec.More() {
		return Evidence{}, EvidenceUnparsed
	}
	if utf8.RuneCountInString(ev.Note) > evidenceNoteMaxRunes {
		return Evidence{}, EvidenceUnparsed
	}
	switch ev.Kind {
	case EvidenceCommit:
		if ev.PlanKey != "" || !fullSHA.MatchString(ev.SHA) {
			return Evidence{}, EvidenceNoCitation
		}
	case EvidencePlanKey:
		if ev.SHA != "" || !evidencePlanKey.MatchString(ev.PlanKey) {
			return Evidence{}, EvidenceNoCitation
		}
	default:
		return Evidence{}, EvidenceNoCitation
	}
	return ev, ""
}

// CleanupEvidenceFile removes a stale evidence file before a run, as
// [CleanupStatusFile] does for the status file.
func CleanupEvidenceFile(workspacePath string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := workspacekit.RemoveSortieFile(workspacePath, EvidenceFileName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("evidence file cleanup failed", slog.String("workspace", workspacePath), slog.Any("error", err))
	}
}
