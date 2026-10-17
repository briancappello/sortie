//go:build unix

package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/toolsocket"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// socketEnvironment is a recording environment whose workspaces reach
// the tools through a socket under stateDir, removed with the
// workspace's environment as the interface requires.
type socketEnvironment struct {
	*environmenttest.Recorder
	stateDir string
}

var _ environment.ToolSocketer = socketEnvironment{}

func (s socketEnvironment) ToolSocketPath(ws environment.Workspace) (string, error) {
	return filepath.Join(s.stateDir, "env", ws.Key, "tools.sock"), nil
}

func (socketEnvironment) GuestToolSocketPath() string { return "/run/sortie/tools.sock" }

func (socketEnvironment) GuestSortieBinaryPath() string { return "/opt/sortie/sortie" }

func (s socketEnvironment) Remove(ctx context.Context, ws environment.Workspace) error {
	if err := s.Recorder.Remove(ctx, ws); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.stateDir, "env", ws.Key))
}

// toolSocketFixture holds a short temporary directory, so socket paths
// stay within the Unix socket address limit, with a workspace root, a
// state directory, and a fake tool server that prints its identity and
// arguments.
type toolSocketFixture struct {
	root     string
	stateDir string
	binary   string
	workflow string
	env      socketEnvironment
}

func newToolSocketFixture(t *testing.T) toolSocketFixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "tsk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := toolSocketFixture{
		root:     filepath.Join(dir, "ws"),
		stateDir: filepath.Join(dir, "st"),
		binary:   filepath.Join(dir, "sortie"),
		workflow: filepath.Join(dir, "WORKFLOW.md"),
	}
	script := "#!/bin/sh\n" +
		`echo "issue=$SORTIE_ISSUE_ID identifier=$SORTIE_ISSUE_IDENTIFIER workspace=$SORTIE_WORKSPACE dispatch=$SORTIE_DISPATCH_ID session=$SORTIE_SESSION_ID args=$*"` + "\n" +
		"cat >/dev/null\n"
	if err := os.WriteFile(f.binary, []byte(script), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	if err := os.WriteFile(f.workflow, []byte("---\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.root, 0o750); err != nil {
		t.Fatal(err)
	}
	f.env = socketEnvironment{Recorder: recordingEnvironment(t), stateDir: f.stateDir}
	return f
}

func (f toolSocketFixture) workspace(t *testing.T, issueID, identifier string) environment.Workspace {
	t.Helper()
	key, err := workspace.SanitizeKey(identifier)
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(f.root, key)
	if err := os.MkdirAll(host, 0o750); err != nil {
		t.Fatal(err)
	}
	return environment.Workspace{Key: key, HostPath: host, GuestPath: "/workspace", IssueID: issueID, Identifier: identifier}
}

func (f toolSocketFixture) params(ws environment.Workspace, dispatchID string) MCPConfigParams {
	return MCPConfigParams{
		BinaryPath:    f.binary,
		WorkflowPath:  f.workflow,
		WorkspacePath: ws.HostPath,
		IssueID:       ws.IssueID,
		Identifier:    ws.Identifier,
		DBPath:        filepath.Join(f.stateDir, "sortie.db"),
		DispatchID:    dispatchID,
		ProcessEnv:    map[string]string{"SORTIE_SESSION_ID": "inherited"},
	}
}

func newSocketRegistry(t *testing.T) *toolsocket.Registry {
	t.Helper()
	r := toolsocket.NewRegistry(discardLogger())
	t.Cleanup(r.Close)
	return r
}

// callToolSocket connects to the socket at path, ends its input, and
// returns what the tool server wrote.
func callToolSocket(t *testing.T, path string) string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial(%s): %v", path, err)
	}
	defer conn.Close() //nolint:errcheck // test
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_ = conn.(*net.UnixConn).CloseWrite()
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func (f toolSocketFixture) socketOf(ws environment.Workspace) string {
	path, _ := f.env.ToolSocketPath(ws)
	return path
}

func TestServeToolSocket_TwoIssuesEachGetTheirOwnIdentity(t *testing.T) {
	t.Parallel()
	f := newToolSocketFixture(t)
	reg := newSocketRegistry(t)
	ws1 := f.workspace(t, "id-1", "VM-1")
	ws2 := f.workspace(t, "id-2", "VM-2")
	for _, ws := range []environment.Workspace{ws1, ws2} {
		served, err := serveToolSocket(reg, f.env, ws, f.params(ws, "d-"+ws.Key))
		if err != nil || !served {
			t.Fatalf("serveToolSocket(%s) = %v, %v; want true, nil", ws.Key, served, err)
		}
	}

	want2 := "issue=id-2 identifier=VM-2 workspace=" + ws2.HostPath + " dispatch=d-VM-2 session= args=mcp-server --workflow " + f.workflow
	if got := callToolSocket(t, f.socketOf(ws2)); got != want2 {
		t.Errorf("VM-2 tool server:\n got %q\nwant %q", got, want2)
	}
	want1 := "issue=id-1 identifier=VM-1 workspace=" + ws1.HostPath + " dispatch=d-VM-1 session= args=mcp-server --workflow " + f.workflow
	if got := callToolSocket(t, f.socketOf(ws1)); got != want1 {
		t.Errorf("VM-1 tool server:\n got %q\nwant %q", got, want1)
	}
}

func TestServeToolSocket_LaterSessionReplacesTheIdentity(t *testing.T) {
	t.Parallel()
	f := newToolSocketFixture(t)
	reg := newSocketRegistry(t)
	ws := f.workspace(t, "id-1", "VM-1")
	for _, dispatch := range []string{"first", "second"} {
		if _, err := serveToolSocket(reg, f.env, ws, f.params(ws, dispatch)); err != nil {
			t.Fatalf("serveToolSocket(%s): %v", dispatch, err)
		}
	}
	if got := callToolSocket(t, f.socketOf(ws)); !strings.Contains(got, "dispatch=second ") {
		t.Errorf("tool server = %q, want the second dispatch", got)
	}
}

func TestServeToolSocket_EnvironmentWithoutSocket(t *testing.T) {
	t.Parallel()
	f := newToolSocketFixture(t)
	ws := f.workspace(t, "id-1", "VM-1")
	served, err := serveToolSocket(newSocketRegistry(t), f.env.Recorder, ws, f.params(ws, "d"))
	if served || err != nil {
		t.Errorf("serveToolSocket(no socket) = %v, %v; want false, nil", served, err)
	}
}

func TestRecoverToolSockets_ServesTheRecordedIdentityAfterRestart(t *testing.T) {
	t.Parallel()
	f := newToolSocketFixture(t)
	ws1 := f.workspace(t, "id-1", "VM-1")
	ws2 := f.workspace(t, "id-2", "VM-2")
	before := toolsocket.NewRegistry(discardLogger())
	for _, ws := range []environment.Workspace{ws1, ws2} {
		if _, err := serveToolSocket(before, f.env, ws, f.params(ws, "d-"+ws.Key)); err != nil {
			t.Fatalf("serveToolSocket(%s): %v", ws.Key, err)
		}
	}
	before.Close()

	// A workspace whose record names another issue: written by hand, as
	// nothing in Sortie writes one.
	ws3 := f.workspace(t, "id-3", "VM-3")
	if err := writeToolSessionRecord(filepath.Dir(f.socketOf(ws3)), toolSessionRecord{IssueID: "id-1", Identifier: "VM-1"}); err != nil {
		t.Fatal(err)
	}
	// A workspace that never served a socket.
	f.workspace(t, "id-4", "VM-4")

	after := newSocketRegistry(t)
	cfg := config.ServiceConfig{Workspace: config.WorkspaceConfig{Root: f.root}}
	served := recoverToolSockets(cfg, toolSocketRecovery{
		Sockets: after, Environment: f.env, BinaryPath: f.binary, WorkflowPath: f.workflow,
		DBPath: filepath.Join(f.stateDir, "sortie.db"), Logger: discardLogger(),
	})
	if strings.Join(served, ",") != "VM-1,VM-2" {
		t.Errorf("recovered %q, want VM-1 and VM-2", served)
	}
	if got := callToolSocket(t, f.socketOf(ws2)); !strings.HasPrefix(got, "issue=id-2 identifier=VM-2 workspace="+ws2.HostPath+" dispatch=d-VM-2 ") {
		t.Errorf("VM-2 tool server after restart = %q", got)
	}
	if after.Serving("VM-3") {
		t.Error("a record naming another issue was served")
	}
}

func TestToolSocket_RemovedWithTheWorkspace(t *testing.T) {
	t.Parallel()
	f := newToolSocketFixture(t)
	reg := newSocketRegistry(t)
	ws := f.workspace(t, "id-1", "VM-1")
	if _, err := serveToolSocket(reg, f.env, ws, f.params(ws, "d")); err != nil {
		t.Fatal(err)
	}
	if err := workspace.CleanupByPath(context.Background(), workspace.CleanupByPathParams{
		Path: ws.HostPath, Identifier: ws.Identifier, IssueID: ws.IssueID, Environment: f.env, Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("CleanupByPath: %v", err)
	}
	if _, err := os.Lstat(f.socketOf(ws)); !os.IsNotExist(err) {
		t.Errorf("socket after removal: err = %v, want not exist", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for reg.Serving(ws.Key) {
		if time.Now().After(deadline) {
			t.Fatal("listener still serving after its workspace was removed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunWorkerAttempt_ServesTheToolSocket(t *testing.T) {
	f := newToolSocketFixture(t)
	reg := newSocketRegistry(t)
	cfg := defaultWorkerConfig(f.root)
	cfg.Agent.MaxTurns = 1

	ec := newExitCapture()
	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		EnvironmentFunc: func(config.EnvironmentConfig) (environment.Environment, error) {
			return f.env, nil
		},
		WorkflowPath:    f.workflow,
		MCPServerBinary: f.binary,
		DispatchID:      "dispatch-7",
		ToolSockets:     reg,
	})
	if res := ec.waitResult(t); res.Error != nil {
		t.Fatalf("worker error = %v", res.Error)
	}

	issue := workerTestIssue()
	ws := environment.Workspace{Key: issue.Identifier}
	got := callToolSocket(t, f.socketOf(ws))
	want := "issue=" + issue.ID + " identifier=" + issue.Identifier + " workspace=" + filepath.Join(f.root, issue.Identifier) + " dispatch=dispatch-7 "
	if !strings.HasPrefix(got, want) {
		t.Errorf("tool server = %q, want prefix %q", got, want)
	}
	rec, found, err := readToolSessionRecord(filepath.Dir(f.socketOf(ws)))
	if err != nil || !found || rec.IssueID != issue.ID || rec.DispatchID != "dispatch-7" {
		t.Errorf("session record = %+v, %v, %v", rec, found, err)
	}
	entry := sortieEntry(t, readMCPConfig(t, filepath.Join(f.root, issue.Identifier)))
	if entry["command"] != "/opt/sortie/sortie" || entry["env"] != nil {
		t.Errorf("sortie-tools = %v, want the guest relay with no env block", entry)
	}
}

// TestGenerateMCPConfig_PerEnvironmentKind generates the session's MCP
// configuration the way the worker does for each kind of launch: on
// this host, over SSH (which runs with the local environment), and in an
// environment that reaches the tools through a socket.
func TestGenerateMCPConfig_PerEnvironmentKind(t *testing.T) {
	t.Parallel()
	local, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: config.EnvironmentKindLocal}})
	if err != nil {
		t.Fatal(err)
	}
	f := newToolSocketFixture(t)
	secret := mcpconfigSecret(t)

	for _, tc := range []struct {
		name  string
		env   environment.Environment
		guest bool
	}{
		{name: "local", env: local},
		{name: "ssh", env: local},
		{name: "socket environment", env: f.env, guest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := mcpParams(dir)
			p.ProcessEnv = map[string]string{"SORTIE_TEST_TOKEN": secret}
			p.GuestCommand = guestToolCommand(tc.env)
			if _, err := GenerateMCPConfig(p); err != nil {
				t.Fatalf("GenerateMCPConfig: %v", err)
			}
			entry := sortieEntry(t, readMCPConfig(t, dir))
			if !tc.guest {
				if entry["command"] != p.BinaryPath {
					t.Errorf("command = %v, want %q", entry["command"], p.BinaryPath)
				}
				env, _ := entry["env"].(map[string]any)
				if env["SORTIE_ISSUE_ID"] != p.IssueID || env["SORTIE_TEST_TOKEN"] != secret {
					t.Errorf("env = %v, want the session identity and process variables", env)
				}
				return
			}
			want := map[string]any{
				"type":    "stdio",
				"command": "/opt/sortie/sortie",
				"args":    []any{"mcp-relay", "/run/sortie/tools.sock"},
			}
			got, _ := json.Marshal(entry)
			wantJSON, _ := json.Marshal(want)
			if string(got) != string(wantJSON) {
				t.Errorf("sortie-tools = %s, want %s", got, wantJSON)
			}
			data, err := os.ReadFile(filepath.Join(dir, ".sortie", "mcp.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, leaked := range []string{secret, p.DBPath, p.WorkflowPath, p.BinaryPath} {
				if strings.Contains(string(data), leaked) {
					t.Errorf("guest mcp.json holds host value %q:\n%s", leaked, data)
				}
			}
			if got := redact.Mask(secret); got == secret {
				t.Errorf("the host tool server's credential stayed unmasked")
			}
		})
	}
}
