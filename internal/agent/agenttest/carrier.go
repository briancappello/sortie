package agenttest

import (
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
)

// FakeCarrier is a [domain.LaunchCarrier] that stands in for an execution
// environment in tests. Its "environment" is this host: the argv it builds
// runs the agent through env(1), which unsets every Unset name and sets
// every Env and AgentEnv entry first, so a fake runtime started through it
// runs and sees what a carried agent would see. It maps paths under
// HostWorkspace to GuestWorkspace, and records every launch. Safe for
// concurrent use.
type FakeCarrier struct {
	// HostWorkspace and GuestWorkspace are the workspace on this host and
	// inside the stand-in environment. GuestPath maps the first to the
	// second; empty HostWorkspace maps nothing.
	HostWorkspace  string
	GuestWorkspace string

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

// GuestPath maps a path under HostWorkspace to the same path under
// GuestWorkspace and returns any other path unchanged.
func (c *FakeCarrier) GuestPath(hostPath string) string {
	if c.HostWorkspace == "" {
		return hostPath
	}
	rel, err := filepath.Rel(c.HostWorkspace, hostPath)
	if err != nil || !filepath.IsLocal(rel) {
		return hostPath
	}
	return path.Join(c.GuestWorkspace, filepath.ToSlash(rel))
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
