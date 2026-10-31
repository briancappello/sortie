//go:build unix

package smolvm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// The test binary doubles as a scripted smolvm: a wrapper script runs it
// with fakeDirEnv naming the directory that holds the fake's state and
// call log.
const fakeDirEnv = "SORTIE_SMOLVM_FAKE_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeDirEnv); dir != "" {
		os.Exit(runFakeSmolvm(dir, os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// fakeMachine is a machine the fake knows, in the listing's shape.
type fakeMachine struct {
	Name    string            `json:"name"`
	State   string            `json:"state"`
	PID     *int              `json:"pid"`
	Labels  map[string]string `json:"labels"`
	Mounts  int               `json:"mounts"`
	Network bool              `json:"network"`
	Image   string            `json:"image"`
}

// fakeFailure is a scripted outcome for one subcommand.
type fakeFailure struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type fakeState struct {
	// Version is what --version prints; "" prints "smolvm 1.32.4".
	Version string `json:"version"`

	Machines []fakeMachine `json:"machines"`

	// Fail scripts the outcome of a subcommand such as "machine start"
	// or "--version" instead of the fake's own behavior.
	Fail map[string]fakeFailure `json:"fail"`

	// ListOutput, when set, is printed verbatim by the machine listing.
	ListOutput string `json:"list_output"`

	// RecordEnv names host variables whose values each call records.
	RecordEnv []string `json:"record_env"`
}

// fakeCall is one recorded invocation.
type fakeCall struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

// fakeSmolvm is a scripted smolvm binary for one test.
type fakeSmolvm struct {
	t      *testing.T
	dir    string
	Binary string
}

func newFakeSmolvm(t *testing.T, state fakeState) *fakeSmolvm {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	dir := t.TempDir()
	f := &fakeSmolvm{t: t, dir: dir}
	f.setState(state)
	f.Binary = agenttest.WriteScript(t, dir, "smolvm",
		fmt.Sprintf("%s=%s exec %s \"$@\"\n", fakeDirEnv, shellQuote(dir), shellQuote(exe)))
	return f
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (f *fakeSmolvm) setState(state fakeState) {
	f.t.Helper()
	if err := writeFakeState(f.dir, state); err != nil {
		f.t.Fatalf("write fake state: %v", err)
	}
}

func (f *fakeSmolvm) state() fakeState {
	f.t.Helper()
	state, err := readFakeState(f.dir)
	if err != nil {
		f.t.Fatalf("read fake state: %v", err)
	}
	return state
}

func (f *fakeSmolvm) calls() []fakeCall {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "calls.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("read fake calls: %v", err)
	}
	var calls []fakeCall
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var c fakeCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			f.t.Fatalf("parse fake call %q: %v", line, err)
		}
		calls = append(calls, c)
	}
	return calls
}

// argvs returns the recorded argument lists.
func (f *fakeSmolvm) argvs() [][]string {
	var out [][]string
	for _, c := range f.calls() {
		out = append(out, c.Args)
	}
	return out
}

func readFakeState(dir string) (fakeState, error) {
	var state fakeState
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func writeFakeState(dir string, state fakeState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600)
}

// runFakeSmolvm behaves like the parts of smolvm 1.32.4 the client drives,
// with the messages and exit codes measured from the real binary.
func runFakeSmolvm(dir string, args []string, stdout, stderr io.Writer) int {
	state, err := readFakeState(dir)
	if err != nil {
		say(stderr, "fake smolvm: %v\n", err)
		return 99
	}
	call := fakeCall{Args: args, Env: map[string]string{}}
	for _, name := range state.RecordEnv {
		if v, ok := os.LookupEnv(name); ok {
			call.Env[name] = v
		}
	}
	line, _ := json.Marshal(call)
	logFile, err := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		say(stderr, "fake smolvm: %v\n", err)
		return 99
	}
	_, _ = logFile.Write(append(line, '\n'))
	_ = logFile.Close()

	key := ""
	switch {
	case len(args) >= 1 && args[0] == "--version":
		key = "--version"
	case len(args) >= 2 && args[0] == "machine":
		key = "machine " + args[1]
	default:
		say(stderr, "fake smolvm: unexpected arguments %q\n", args)
		return 2
	}
	if failure, ok := state.Fail[key]; ok {
		say(stdout, "%s", failure.Stdout)
		say(stderr, "%s", failure.Stderr)
		return failure.Exit
	}
	if key == "--version" {
		version := state.Version
		if version == "" {
			version = "smolvm 1.32.4"
		}
		say(stdout, "%s\n", version)
		return 0
	}

	flags, rest := parseFakeFlags(args[2:])
	name := flags.last("--name")
	idx := slices.IndexFunc(state.Machines, func(m fakeMachine) bool { return m.Name == name })
	notFound := func() int {
		say(stderr, "Error: vm not found: %s\n", name)
		return 1
	}

	switch args[1] {
	case "ls":
		if state.ListOutput != "" {
			say(stdout, "%s", state.ListOutput)
			return 0
		}
		machines := state.Machines
		if machines == nil {
			machines = []fakeMachine{}
		}
		data, _ := json.MarshalIndent(machines, "", "  ")
		say(stdout, "%s\n", string(data))
		return 0
	case "create":
		if idx >= 0 {
			say(stderr, "Error: machine already exists: %s\n", name)
			return 1
		}
		m := fakeMachine{Name: name, State: StateCreated, Labels: map[string]string{}, Image: flags.last("--from")}
		for _, l := range flags.all("--label") {
			k, v, _ := strings.Cut(l, "=")
			m.Labels[k] = v
		}
		m.Mounts = len(flags.all("-v"))
		m.Network = len(flags.all("--allow-host-pattern")) > 0 || len(flags.all("--credential")) > 0
		state.Machines = append(state.Machines, m)
		say(stdout, "Created machine: %s\n", name)
	case "start":
		if idx < 0 {
			return notFound()
		}
		pid := 4242
		state.Machines[idx].State = StateRunning
		state.Machines[idx].PID = &pid
		say(stdout, "Machine '%s' running (PID: %d)\n", name, pid)
	case "stop":
		if idx < 0 {
			return notFound()
		}
		if state.Machines[idx].State != StateRunning {
			say(stdout, "Machine '%s' is not running (state: %s)\n", name, state.Machines[idx].State)
			return 0
		}
		state.Machines[idx].State = StateStopped
		state.Machines[idx].PID = nil
	case "delete":
		if idx < 0 {
			return notFound()
		}
		state.Machines = slices.Delete(state.Machines, idx, idx+1)
		say(stdout, "Deleted machine: %s\n", name)
	case "exec":
		if idx < 0 {
			return notFound()
		}
		if state.Machines[idx].State != StateRunning {
			say(stderr, "Error: agent operation failed: connect: machine '%s' is not running. Start it with 'smolvm machine start --name %s'; if a start is already in progress, retry once it finishes.\n", name, name)
			return 1
		}
		say(stdout, "%s\n", strings.Join(rest, " "))
		return 0
	default:
		say(stderr, "fake smolvm: unsupported subcommand %q\n", args[1])
		return 2
	}
	if err := writeFakeState(dir, state); err != nil {
		say(stderr, "fake smolvm: %v\n", err)
		return 99
	}
	return 0
}

// fakeFlags holds parsed flag values in order.
type fakeFlags map[string][]string

func (f fakeFlags) all(name string) []string { return f[name] }

func (f fakeFlags) last(name string) string {
	values := f[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// parseFakeFlags splits smolvm flags from the arguments after "--".
func parseFakeFlags(args []string) (fakeFlags, []string) {
	valued := map[string]bool{
		"--name": true, "-n": true, "--from": true, "--cpus": true, "--mem": true, "--storage": true,
		"--label": true, "--allow-host-pattern": true, "--credential": true, "-v": true,
		"--mount-socket": true, "-w": true, "-e": true, "--secret-env": true,
	}
	flags := fakeFlags{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return flags, args[i+1:]
		}
		if valued[arg] && i+1 < len(args) {
			name := arg
			if name == "-n" {
				name = "--name"
			}
			flags[name] = append(flags[name], args[i+1])
			i++
			continue
		}
		flags[arg] = append(flags[arg], "")
	}
	return flags, nil
}

// say writes to the fake's output, where a failed write has nowhere to
// be reported.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
