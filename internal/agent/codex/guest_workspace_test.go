package codex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// carriedTarget is a launch target running in a stand-in execution
// environment whose guest workspace is /workspace.
func carriedTarget(hostWorkspace string) agentcore.LaunchTarget {
	return agentcore.LaunchTarget{
		WorkspacePath: hostWorkspace,
		Carrier:       &agenttest.FakeCarrier{HostWorkspace: hostWorkspace, GuestWorkspace: "/workspace"},
	}
}

// TestStartThread_CwdIsTheGuestWorkspaceInAnEnvironment asserts that
// thread/start names the workspace as the runtime sees it: inside an
// execution environment the host path does not exist.
func TestStartThread_CwdIsTheGuestWorkspaceInAnEnvironment(t *testing.T) {
	t.Parallel()

	stdin := &capturingWriteCloser{}
	state := handshakeStateWithRecorder(t, stdin,
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)
	state.target = carriedTarget(t.TempDir())

	if _, _, err := startThread(context.Background(), state, passthroughConfig{}, discardTestLogger()); err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	write, ok := stdin.find(`{"method":"thread/start"`)
	if !ok {
		t.Fatal("startThread() wrote no thread/start request")
	}
	var req struct {
		Params struct {
			Cwd string `json:"cwd"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(write), &req); err != nil {
		t.Fatalf("unmarshal thread/start request: %v", err)
	}
	if req.Params.Cwd != "/workspace" {
		t.Errorf("thread/start cwd = %q, want %q", req.Params.Cwd, "/workspace")
	}
}

// TestRunTurn_CwdAndWritableRootAreTheGuestWorkspaceInAnEnvironment
// asserts that turn/start names the guest workspace as its cwd and as
// the sandbox's writable root inside an execution environment.
func TestRunTurn_CwdAndWritableRootAreTheGuestWorkspaceInAnEnvironment(t *testing.T) {
	t.Parallel()

	stdin := &capturingWriteCloser{}
	state := makeTestStateWithStdin(t, loadFixture(t, "runturn_misc_notifications.jsonl"), stdin)
	state.target = carriedTarget(t.TempDir())
	adapter, _ := NewCodexAdapter()

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	write, ok := stdin.find(`{"method":"turn/start"`)
	if !ok {
		t.Fatal("RunTurn() wrote no turn/start request")
	}
	var req struct {
		Params struct {
			Cwd           string `json:"cwd"`
			SandboxPolicy struct {
				WritableRoots []string `json:"writableRoots"`
			} `json:"sandboxPolicy"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(write), &req); err != nil {
		t.Fatalf("unmarshal turn/start request: %v", err)
	}
	if req.Params.Cwd != "/workspace" {
		t.Errorf("turn/start cwd = %q, want %q", req.Params.Cwd, "/workspace")
	}
	if len(req.Params.SandboxPolicy.WritableRoots) != 1 || req.Params.SandboxPolicy.WritableRoots[0] != "/workspace" {
		t.Errorf("turn/start sandboxPolicy.writableRoots = %q, want [/workspace]", req.Params.SandboxPolicy.WritableRoots)
	}
}
