package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// EnvironmentRecordFile is the name of the file inside the workspace's
// .sortie directory that records which execution environment kind
// created the workspace.
const EnvironmentRecordFile = "environment"

// environmentRecordMaxBytes bounds the record read. A record is a kind
// and an instance name, well under 200 bytes.
const environmentRecordMaxBytes = 4096

// EnvironmentRecord is the content of the environment record: the kind
// that created the workspace and the name of the instance it runs in.
type EnvironmentRecord struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

// recordState describes what [readEnvironmentRecord] found.
type recordState int

const (
	// recordAbsent: no .sortie directory or no record in it. The
	// workspace counts as created by the local kind.
	recordAbsent recordState = iota
	// recordValid: a record naming a kind.
	recordValid
	// recordUnusable: a record that is refused, oversized, malformed,
	// or names no kind.
	recordUnusable
)

// readEnvironmentRecord reads the environment record of the workspace
// at path through [workspacekit], which refuses links and caps the size,
// because the .sortie directory is writable by the agent.
func readEnvironmentRecord(path string) (EnvironmentRecord, recordState, error) {
	data, err := workspacekit.ReadSortieFile(path, EnvironmentRecordFile, environmentRecordMaxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return EnvironmentRecord{}, recordAbsent, nil
	}
	if err != nil {
		return EnvironmentRecord{}, recordUnusable, err
	}
	var rec EnvironmentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return EnvironmentRecord{}, recordUnusable, fmt.Errorf("parse %s: %w", EnvironmentRecordFile, err)
	}
	if rec.Kind == "" {
		return EnvironmentRecord{}, recordUnusable, fmt.Errorf("%s names no kind", EnvironmentRecordFile)
	}
	return rec, recordValid, nil
}

// writeEnvironmentRecord replaces the environment record of the
// workspace at path with rec, creating the .sortie directory when it is
// absent.
func writeEnvironmentRecord(path string, rec EnvironmentRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	dir, err := workspacekit.OpenSortieDir(path, true)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // the directory handle is not needed once the write completes
	return workspacekit.ReplaceFile(dir, EnvironmentRecordFile, data)
}

// environmentKind returns the kind of env; a nil env runs commands on
// the host, which is the local kind.
func environmentKind(env environment.Environment) string {
	if env == nil {
		return config.EnvironmentKindLocal
	}
	return env.Kind()
}

// wantedEnvironmentRecord returns the record a workspace prepared by env
// carries. The local kind writes none: a workspace without a record
// counts as local, and a local workspace stays as it always was, with
// no .sortie directory before after_create runs.
func wantedEnvironmentRecord(env environment.Environment, ws environment.Workspace) (EnvironmentRecord, bool) {
	kind := environmentKind(env)
	if kind == config.EnvironmentKindLocal {
		return EnvironmentRecord{}, false
	}
	rec := EnvironmentRecord{Kind: kind}
	if namer, ok := env.(environment.Namer); ok {
		rec.Name = namer.InstanceName(ws)
	}
	return rec, true
}

// recreateStaleWorkspace removes the existing workspace at ensured.Path
// when the environment kind that created it differs from the configured
// one, and creates the directory again, so preparation starts from
// scratch and after_create runs. It returns ensured unchanged when the
// workspace is current.
//
// The recorded kind is the record's kind, or local without a record.
// For a stale workspace, the recorded kind's environment, built by
// params.EnvironmentForKind, removes its instance; then the host
// directory goes. An unusable record is stale too: its kind is unknown,
// so the configured kind's environment removes whatever instance it
// holds for the workspace. A recorded kind that cannot be built, or
// whose removal fails, fails the preparation and keeps the workspace, so
// no instance is left behind without a record naming it.
func recreateStaleWorkspace(ctx context.Context, logger *slog.Logger, params PrepareParams, ensured EnsureResult) (EnsureResult, error) {
	if ensured.CreatedNow {
		return ensured, nil
	}
	configured := environmentKind(params.Environment)
	rec, state, readErr := readEnvironmentRecord(ensured.Path)
	recorded := config.EnvironmentKindLocal
	switch state {
	case recordValid:
		recorded = rec.Kind
	case recordUnusable:
		recorded = configured
	}
	if state != recordUnusable && recorded == configured {
		return ensured, nil
	}

	attrs := []any{
		slog.String("workspace", ensured.Path),
		slog.String("recorded_environment", recorded),
		slog.String("environment", configured),
	}
	if readErr != nil {
		attrs = append(attrs, slog.Any("record_error", readErr))
	}
	logger.InfoContext(ctx, "workspace was created by another execution environment, recreating it", attrs...)

	staleEnv := params.Environment
	if recorded != configured {
		var err error
		staleEnv, err = environmentForKind(params, recorded)
		if err != nil {
			return EnsureResult{}, fmt.Errorf("remove workspace created by environment %q: %w", recorded, err)
		}
	}
	stale := NewTarget(staleEnv, ensured.Key, ensured.Path, params.IssueID, params.Identifier)
	if err := removeEnvironment(ctx, logger, stale); err != nil {
		return EnsureResult{}, err
	}
	if err := os.RemoveAll(ensured.Path); err != nil {
		return EnsureResult{}, fmt.Errorf("remove stale workspace %q: %w", ensured.Path, err)
	}

	recreated, err := Ensure(params.Root, params.Identifier)
	if err != nil {
		return EnsureResult{}, err
	}
	if !recreated.CreatedNow {
		return EnsureResult{}, fmt.Errorf("recreate workspace %q: the directory reappeared before it was created", recreated.Path)
	}
	return recreated, nil
}

// environmentForKind returns the environment of kind for removing a
// stale workspace. The local kind needs none when no builder is given:
// its workspace is the host directory alone.
func environmentForKind(params PrepareParams, kind string) (environment.Environment, error) {
	if params.EnvironmentForKind != nil {
		return params.EnvironmentForKind(kind)
	}
	if kind == config.EnvironmentKindLocal {
		return nil, nil
	}
	return nil, fmt.Errorf("no way to build environment %q", kind)
}

// removalTarget returns the target that removes the workspace at path,
// whose before_remove hook runs in it.
//
// A workspace whose record names a kind other than local and other
// than the configured one was created by that kind: forKind builds it,
// and it removes the workspace, so switching the configured kind does
// not orphan the instance. Every other workspace is removed by the
// configured environment. A record naming local, or no record, never
// moves removal to the host: the agent of a kind that runs elsewhere
// can write or delete the record through the shared control directory.
// A nil forKind always selects the configured environment.
func removalTarget(configured environment.Environment, forKind func(kind string) (environment.Environment, error), key, path, issueID, identifier string) (Target, error) {
	env := configured
	if forKind != nil {
		rec, state, _ := readEnvironmentRecord(path)
		if state == recordValid && rec.Kind != config.EnvironmentKindLocal && rec.Kind != environmentKind(configured) {
			built, err := forKind(rec.Kind)
			if err != nil {
				return Target{}, fmt.Errorf("remove workspace created by environment %q: %w", rec.Kind, err)
			}
			env = built
		}
	}
	return NewTarget(env, key, path, issueID, identifier), nil
}

// ensureEnvironmentRecord writes want as the record of the workspace at
// path unless the record already holds it.
func ensureEnvironmentRecord(path string, want EnvironmentRecord) error {
	if rec, state, _ := readEnvironmentRecord(path); state == recordValid && rec == want {
		return nil
	}
	return writeEnvironmentRecord(path, want)
}
