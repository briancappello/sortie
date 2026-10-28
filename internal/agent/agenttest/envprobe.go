package agenttest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// EnvProbeScenario names the built-in [Scenario] that runs [EnvProbe.Run].
const EnvProbeScenario = "agenttest.envprobe"

// EnvProbe parameterizes [EnvProbeScenario]. Every launch appends one
// [EnvProbeLaunch] line to Path. A launch whose arguments include
// --version then writes Version and a newline and exits 0 when Version
// is non-empty; every other launch stays alive until it is killed.
type EnvProbe struct {
	Path    string
	Names   []string
	Version string
}

// EnvProbeLaunch is one launch an [EnvProbe] recorded.
type EnvProbeLaunch struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

// Run records the launch and then behaves as [EnvProbe] describes.
func (p EnvProbe) Run(args []string) int {
	launch := EnvProbeLaunch{Args: args, Env: make(map[string]string, len(p.Names))}
	for _, name := range p.Names {
		if value, ok := os.LookupEnv(name); ok {
			launch.Env[name] = value
		}
	}
	data, err := json.Marshal(launch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: encode probe: %v\n", err)
		return 2
	}
	f, err := os.OpenFile(p.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: open probe log: %v\n", err)
		return 2
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		fmt.Fprintf(os.Stderr, "agenttest: write probe log: %v %v\n", writeErr, closeErr)
		return 2
	}
	if p.Version != "" && slices.Contains(args, "--version") {
		return Output{Stdout: p.Version + "\n"}.Run()
	}
	Hang()
	return 0
}

func runEnvProbe(args []string, params EnvProbe) int {
	return params.Run(args)
}

// ReadEnvProbe returns the launches recorded at path so far, in order.
func ReadEnvProbe(t testing.TB, path string) []EnvProbeLaunch {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // the test names the probe log it created
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open probe log: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle
	var launches []EnvProbeLaunch
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var launch EnvProbeLaunch
		if err := json.Unmarshal(scanner.Bytes(), &launch); err != nil {
			continue
		}
		launches = append(launches, launch)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	return launches
}

// AwaitEnvProbe polls path until a recorded launch satisfies match and
// returns it, failing the test after 60 seconds.
func AwaitEnvProbe(t testing.TB, path string, match func(EnvProbeLaunch) bool) EnvProbeLaunch {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, launch := range ReadEnvProbe(t, path) {
			if match(launch) {
				return launch
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no matching launch recorded at %s within 60s; recorded: %+v", path, ReadEnvProbe(t, path))
	return EnvProbeLaunch{}
}
