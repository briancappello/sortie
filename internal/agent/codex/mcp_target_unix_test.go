//go:build unix

package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// TestMCPOverridesLocal asserts that a local launch passes the generated
// stdio tool server to the runtime as an override argument rendered from
// the host copy of the file, so the path itself never reaches the runtime.
func TestMCPOverridesLocal(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	mcpPath := filepath.Join(ws, ".sortie", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(mcpPath), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server"]}}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	params := domain.StartSessionParams{WorkspacePath: ws, AgentConfig: domain.AgentConfig{Command: "sh -c"}, MCPConfigPath: mcpPath}
	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
	}
	tools, agentErr := agentcore.ResolveToolDelivery(context.Background(), params, target, registry.MCPDialectMCPServersTOML, nil)
	if agentErr != nil {
		t.Fatalf("ResolveToolDelivery() error = %v", agentErr)
	}
	t.Cleanup(func() { _ = tools.Close() })
	args := tools.Delivery(sshutil.ForwardOutcome{}).Args
	if !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "mcp_servers.sortie-tools=") }) {
		t.Errorf("delivery args = %q, want the sortie-tools override", args)
	}
}

// TestMCPOverridesCarrier asserts that a carried launch names Sortie's tool
// server at the tool channel's guest port in the overrides it appends to the
// app-server command, and passes the bearer by name only.
func TestMCPOverridesCarrier(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	carrier := &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace", Channel: agenttest.NewToolChannel(t)}
	params := domain.StartSessionParams{
		WorkspacePath: ws,
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
		MCPConfigPath: agenttest.WriteHTTPToolServerConfig(t, ws),
		Carrier:       carrier,
	}
	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
	}
	tools, agentErr := agentcore.ResolveToolDelivery(context.Background(), params, target, registry.MCPDialectMCPServersTOML, nil)
	if agentErr != nil {
		t.Fatalf("ResolveToolDelivery() error = %v", agentErr)
	}
	t.Cleanup(func() { _ = tools.Close() })

	launch, agentErr := target.ToolSessionLaunch(nil, tools.Launch(placeToolServers))
	if agentErr != nil {
		t.Fatalf("ToolSessionLaunch() error = %v", agentErr)
	}
	launches := carrier.Launches()
	if len(launches) != 1 {
		t.Fatalf("carrier launches = %d, want 1", len(launches))
	}
	argv := strings.Join(launches[0].Argv, " ")
	if !strings.Contains(argv, "127.0.0.1:47800") {
		t.Errorf("carrier argv = %q, want Sortie's tool server at the guest port", launches[0].Argv)
	}
	if !slices.Contains(launches[0].SecretEnv, mcpconfig.BearerEnv) {
		t.Errorf("SecretEnv = %q, want %s", launches[0].SecretEnv, mcpconfig.BearerEnv)
	}
	if !slices.ContainsFunc(launch.Env, func(e string) bool { return strings.HasPrefix(e, mcpconfig.BearerEnv+"=") }) {
		t.Errorf("HostLaunch.Env = %q, want the bearer value for the carrier process", launch.Env)
	}
}
