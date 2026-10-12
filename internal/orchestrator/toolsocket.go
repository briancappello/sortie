package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/toolsocket"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// toolSessionFile names the record, next to a workspace's tool socket,
// of the session identity the socket serves. The socket's directory is
// on the host, out of the environment's reach, so a restarted Sortie
// takes the issue's identity from it and not from the workspace's
// .sortie directory, which the agent can write.
const (
	toolSessionFile     = "tool-session.json"
	toolSessionMaxBytes = 4096
)

// toolSessionRecord is the per-session part of the tool server's env
// block. Everything else in it, credentials included, is derived from
// the configuration in force when the socket is served, and is never
// written down.
type toolSessionRecord struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"identifier"`
	DispatchID string `json:"dispatch_id,omitempty"`
	Attempt    *int   `json:"attempt,omitempty"`
	AgentKind  string `json:"agent_kind,omitempty"`
}

// serveToolSocket makes the host listen on ws's tool socket and run the
// tool server params describes, on the host and with params' env block,
// for every connection, when env reaches Sortie's tools through a
// socket. It records the session identity next to the socket first, so
// a restart serves the same issue. It reports whether env has a tool
// socket. A nil registry serves nothing.
func serveToolSocket(sockets *toolsocket.Registry, env environment.Environment, ws environment.Workspace, params MCPConfigParams) (bool, error) {
	socketer, ok := env.(environment.ToolSocketer)
	if !ok || sockets == nil {
		return false, nil
	}
	path, err := socketer.ToolSocketPath(ws)
	if err != nil {
		return true, err
	}
	if err := writeToolSessionRecord(filepath.Dir(path), toolSessionRecord{
		IssueID:    params.IssueID,
		Identifier: params.Identifier,
		DispatchID: params.DispatchID,
		Attempt:    params.Attempt,
		AgentKind:  params.AgentKind,
	}); err != nil {
		return true, err
	}
	return true, sockets.Serve(ws.Key, path, toolServerSpec(params))
}

// toolServerSpec returns the host command a tool socket starts for each
// connection: the same command and env block the generated MCP
// configuration names for an agent on this host. The process inherits
// this process's environment except its SORTIE_* variables, which come
// from the env block alone.
func toolServerSpec(params MCPConfigParams) toolsocket.Spec {
	block := toolServerEnv(params)
	env := make([]string, 0, len(os.Environ())+len(block))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SORTIE_") {
			env = append(env, entry)
		}
	}
	names := make([]string, 0, len(block))
	for name := range block {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+block[name])
	}
	return toolsocket.Spec{Argv: append([]string{params.BinaryPath}, toolServerArgs(params)...), Env: env}
}

func writeToolSessionRecord(dir string, rec toolSessionRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode tool session record: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create tool socket directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, toolSessionFile+".*")
	if err != nil {
		return fmt.Errorf("write tool session record: %w", err)
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write tool session record: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, toolSessionFile)); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write tool session record: %w", err)
	}
	return nil
}

// readToolSessionRecord reads the record in dir. A missing record is
// (zero, false, nil).
func readToolSessionRecord(dir string) (toolSessionRecord, bool, error) {
	path := filepath.Join(dir, toolSessionFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return toolSessionRecord{}, false, nil
	}
	if err != nil {
		return toolSessionRecord{}, false, err
	}
	if !info.Mode().IsRegular() {
		return toolSessionRecord{}, false, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: Sortie's own state directory
	if err != nil {
		return toolSessionRecord{}, false, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	data, err := io.ReadAll(io.LimitReader(f, toolSessionMaxBytes+1))
	if err != nil {
		return toolSessionRecord{}, false, err
	}
	if len(data) > toolSessionMaxBytes {
		return toolSessionRecord{}, false, fmt.Errorf("%s is larger than %d bytes", path, toolSessionMaxBytes)
	}
	var rec toolSessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return toolSessionRecord{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return rec, true, nil
}

// toolSocketRecovery holds what [recoverToolSockets] needs besides the
// configuration.
type toolSocketRecovery struct {
	Sockets      *toolsocket.Registry
	Environment  environment.Environment
	BinaryPath   string // explicit tool server binary; "" is the running executable
	WorkflowPath string
	DBPath       string
	Logger       *slog.Logger
}

// recoverToolSockets listens again, after a restart, on the tool socket
// of every workspace under cfg's workspace root that served one, with
// the identity recorded next to the socket, so a VM still running
// reaches the new listener. A record whose identifier does not belong to
// the workspace is skipped. It returns the keys it serves.
func recoverToolSockets(cfg config.ServiceConfig, p toolSocketRecovery) []string {
	socketer, ok := p.Environment.(environment.ToolSocketer)
	if !ok || p.Sockets == nil || p.WorkflowPath == "" {
		return nil
	}
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	entries, err := os.ReadDir(cfg.Workspace.Root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("tool socket recovery: cannot list the workspace root",
				slog.String("workspace_root", cfg.Workspace.Root), slog.Any("error", err))
		}
		return nil
	}
	binary, err := resolveToolServerBinary(p.BinaryPath)
	if err != nil {
		logger.Warn("tool socket recovery: cannot resolve the tool server binary", slog.Any("error", err))
		return nil
	}
	processEnv := CollectSortieEnv()
	var served []string
	for _, entry := range entries {
		key := entry.Name()
		if !entry.IsDir() {
			continue
		}
		if sanitized, err := workspace.SanitizeKey(key); err != nil || sanitized != key {
			continue
		}
		hostPath := filepath.Join(cfg.Workspace.Root, key)
		ws := environment.Workspace{Key: key, HostPath: hostPath, GuestPath: p.Environment.GuestPath(hostPath)}
		path, err := socketer.ToolSocketPath(ws)
		if err != nil {
			continue
		}
		rec, found, err := readToolSessionRecord(filepath.Dir(path))
		if err != nil {
			logger.Warn("tool socket recovery: unreadable session record",
				slog.String("workspace", hostPath), slog.Any("error", err))
			continue
		}
		if !found {
			continue
		}
		if recKey, err := workspace.SanitizeKey(rec.Identifier); err != nil || recKey != key || rec.IssueID == "" {
			logger.Warn("tool socket recovery: session record does not belong to the workspace",
				slog.String("workspace", hostPath), slog.String("identifier", rec.Identifier))
			continue
		}
		ws.IssueID, ws.Identifier = rec.IssueID, rec.Identifier
		params := MCPConfigParams{
			BinaryPath:    binary,
			WorkflowPath:  p.WorkflowPath,
			WorkspacePath: hostPath,
			IssueID:       rec.IssueID,
			Identifier:    rec.Identifier,
			DBPath:        p.DBPath,
			DispatchID:    rec.DispatchID,
			Attempt:       rec.Attempt,
			AgentKind:     rec.AgentKind,
			ProcessEnv:    processEnv,
		}
		if err := p.Sockets.Serve(key, path, toolServerSpec(params)); err != nil {
			logger.Warn("tool socket recovery: cannot listen",
				slog.String("workspace", hostPath), slog.Any("error", err))
			continue
		}
		served = append(served, key)
	}
	if len(served) > 0 {
		logger.Info("tool sockets listening again", slog.Any("workspaces", served))
	}
	return served
}
