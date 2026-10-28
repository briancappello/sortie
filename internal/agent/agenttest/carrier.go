package agenttest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// FakeCarrier is a [domain.LaunchCarrier] that stands in for an execution
// environment in tests. Its "environment" is this host: the argv it builds
// runs the agent through env(1), which unsets every Unset name and sets
// every Env and AgentEnv entry first, so a fake runtime started through it
// runs and sees what a carried agent would see; a SecretEnv name reaches it
// from the carrier process's own environment, which env(1) inherits. It
// records every launch. Safe for concurrent use.
type FakeCarrier struct {
	// HostWorkspace and GuestWorkspace are the workspace on this host and
	// inside the stand-in environment. Workspace returns GuestWorkspace.
	HostWorkspace  string
	GuestWorkspace string

	// Channel is what ToolChannel returns; nil reports no channel.
	Channel *domain.ToolChannel

	// Resolve replaces an argv[0] it names with the host path to run, so
	// a test can resolve an agent command to a fake runtime.
	Resolve map[string]string

	// ConnectionFailedExit is the exit status ConnectionFailed reports as
	// a connection failure, when the stderr tail also holds a line
	// containing ConnectionFailedStderr. Zero reports none.
	ConnectionFailedExit   int
	ConnectionFailedStderr string

	// Err fails every LaunchArgv call.
	Err error

	mu       sync.Mutex
	launches []domain.CarrierLaunch
	tails    [][]string
}

var _ domain.LaunchCarrier = (*FakeCarrier)(nil)

// LaunchArgv records launch and returns the env(1) argv that runs it.
func (c *FakeCarrier) LaunchArgv(launch domain.CarrierLaunch) ([]string, error) {
	c.mu.Lock()
	c.launches = append(c.launches, launch)
	c.mu.Unlock()
	if c.Err != nil {
		return nil, c.Err
	}
	envPath, err := exec.LookPath("env")
	if err != nil {
		envPath = "/usr/bin/env"
	}
	argv := []string{envPath}
	for _, name := range launch.Unset {
		argv = append(argv, "-u", name)
	}
	argv = append(argv, launch.Env...)
	argv = append(argv, launch.AgentEnv...)
	agent := slices.Clone(launch.Argv)
	if resolved, ok := c.Resolve[agent[0]]; ok {
		agent[0] = resolved
	}
	return append(argv, agent...), nil
}

// Workspace returns GuestWorkspace.
func (c *FakeCarrier) Workspace() string { return c.GuestWorkspace }

// ToolChannel returns Channel, and false when it is nil.
func (c *FakeCarrier) ToolChannel() (domain.ToolChannel, bool) {
	if c.Channel == nil {
		return domain.ToolChannel{}, false
	}
	return *c.Channel, true
}

// ConnectionFailed records stderrTail and reports whether exitCode and
// stderrTail match ConnectionFailedExit and ConnectionFailedStderr.
func (c *FakeCarrier) ConnectionFailed(exitCode int, stderrTail []string) bool {
	c.mu.Lock()
	c.tails = append(c.tails, slices.Clone(stderrTail))
	c.mu.Unlock()
	if c.ConnectionFailedExit == 0 || exitCode != c.ConnectionFailedExit {
		return false
	}
	return slices.ContainsFunc(stderrTail, func(line string) bool {
		return strings.Contains(line, c.ConnectionFailedStderr)
	})
}

// Launches returns a copy of every launch LaunchArgv received, in order.
func (c *FakeCarrier) Launches() []domain.CarrierLaunch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.launches)
}

// StderrTails returns a copy of every stderr tail ConnectionFailed
// received, in order.
func (c *FakeCarrier) StderrTails() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.tails)
}

// NewToolChannel returns a tool channel whose host socket lies in a new
// short directory, under the length a Unix socket path allows, at guest
// port 47800. The directory is removed when t ends.
func NewToolChannel(t testing.TB) *domain.ToolChannel {
	t.Helper()
	dir, err := os.MkdirTemp("", "tc")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return &domain.ToolChannel{HostSocket: filepath.Join(dir, "tools.sock"), GuestPort: 47800}
}

// WriteHTTPToolServerConfig writes workspace/.sortie/mcp.json naming one
// server, sortie-tools, a fake runtime of [HTTPToolServerScenario], as the
// orchestrator names Sortie's tool server, and returns its path.
func WriteHTTPToolServerConfig(t testing.TB, workspace string) string {
	t.Helper()
	server := FakeRuntime(t, t.TempDir(), "sortie", HTTPToolServerScenario, struct{}{})
	path := filepath.Join(workspace, workspacekit.SortieDir, "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	doc := `{"mcpServers":{"sortie-tools":{"type":"stdio","command":` + strconv.Quote(server) + `,"args":["mcp-server"]}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}
