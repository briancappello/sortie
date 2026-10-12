package smolvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// DefaultBinary is the smolvm program run when the configuration names
// none.
const DefaultBinary = "smolvm"

// SupportedVersion is the smolvm release range this package drives.
// Every flag, JSON field, and error text the client relies on was
// measured against it.
const SupportedVersion = "1.26.x"

const (
	supportedMajor = 1
	supportedMinor = 26

	// stderrTailBytes is how much of a failed invocation's standard
	// error a [CLIError] keeps: the same tail a workspace hook keeps.
	stderrTailBytes = 8 * 1024

	// maxStdoutBytes bounds the standard output the client reads from
	// one invocation. A machine listing takes about 1.5 KiB per machine.
	maxStdoutBytes = 4 << 20
)

var (
	// ErrOutputTooLarge reports that an invocation wrote more standard
	// output than the client reads.
	ErrOutputTooLarge = errors.New("smolvm output exceeds the read limit")

	errOutputIncomplete = errors.New("smolvm output did not complete after the process exited")
)

// CLIError reports a smolvm invocation that failed: it could not be
// started, it was killed, or it exited non-zero.
type CLIError struct {
	// Args holds the subcommand words, such as ["machine", "start"].
	// Flag values never appear in it.
	Args []string

	// Machine names the machine the invocation addressed, if any.
	Machine string

	// ExitCode is the process exit code, or -1 when the process did
	// not exit on its own: it could not be started, or it was killed.
	ExitCode int

	// Stderr holds the last 8 KiB of the invocation's standard error,
	// with every registered secret value masked.
	Stderr string

	// Err is the underlying error from running the process.
	Err error
}

func (e *CLIError) Error() string {
	var b strings.Builder
	b.WriteString("smolvm")
	for _, arg := range e.Args {
		b.WriteByte(' ')
		b.WriteString(arg)
	}
	if e.Machine != "" {
		fmt.Fprintf(&b, " (machine %q)", e.Machine)
	}
	if e.ExitCode >= 0 {
		fmt.Fprintf(&b, ": exit code %d", e.ExitCode)
	} else if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if summary := stderrSummary(e.Stderr); summary != "" {
		b.WriteString(": ")
		b.WriteString(summary)
	}
	return b.String()
}

func (e *CLIError) Unwrap() error { return e.Err }

// stderrSummary returns the line of stderr that best describes a
// failure: the last line starting with "Error:", else the last
// non-blank line.
func stderrSummary(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for _, line := range slices.Backward(lines) {
		if line := strings.TrimSpace(line); strings.HasPrefix(line, "Error:") {
			return line
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// IsNotFound reports whether err is a [CLIError] from an invocation that
// addressed a machine smolvm does not know.
func IsNotFound(err error) bool {
	var cliErr *CLIError
	return errors.As(err, &cliErr) && cliErr.ExitCode == 1 && strings.Contains(cliErr.Stderr, "vm not found:")
}

// Machine states reported by the machine listing.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateCreated = "created"
)

// Machine is one entry of the machine listing, reduced to the fields
// Sortie reads.
type Machine struct {
	Name   string            `json:"name"`
	State  string            `json:"state"`
	PID    *int              `json:"pid"`
	Labels map[string]string `json:"labels"`

	// Mounts counts the machine's directory mounts. A mounted socket is
	// not counted.
	Mounts int `json:"mounts"`

	// Network reports whether the machine has outbound network access.
	Network bool `json:"network"`
}

// Running reports whether the machine is running.
func (m Machine) Running() bool { return m.State == StateRunning }

// parseMachines parses the output of the machine listing.
func parseMachines(data []byte) ([]Machine, error) {
	var machines []Machine
	if err := json.Unmarshal(data, &machines); err != nil {
		return nil, fmt.Errorf("parse smolvm machine listing: %w", err)
	}
	return machines, nil
}

// Volume is a host directory mounted into a machine.
type Volume struct {
	Host     string
	Guest    string
	ReadOnly bool
}

// SocketMount is a host Unix socket mounted into a machine.
type SocketMount struct {
	Host  string
	Guest string
}

// CredentialBinding binds the host variable Env to the guest variable of
// the same name for Hosts, under the credential name Name.
type CredentialBinding struct {
	Name  string
	Env   string
	Hosts []string
}

// Label is one machine label.
type Label struct {
	Key   string
	Value string
}

// CreateSpec describes a machine to create from a pack.
type CreateSpec struct {
	Name string
	Pack string

	// CPUs, MemoryMiB, and StorageGiB size the machine; 0 keeps the
	// pack's or smolvm's default.
	CPUs       int
	MemoryMiB  int
	StorageGiB int

	Labels []Label

	// AllowHosts lists the egress host patterns. Nothing else opens the
	// network: without it and without Credentials the machine has none.
	AllowHosts []string

	Credentials []CredentialBinding
	SSHAgent    bool
	Volumes     []Volume
	Sockets     []SocketMount
}

// createArgs returns the arguments of the create invocation for spec.
// A path that contains the mount separator ':' is refused, since smolvm
// would split it.
func createArgs(spec CreateSpec) ([]string, error) {
	if spec.Name == "" || spec.Pack == "" {
		return nil, errors.New("smolvm machine create needs a name and a pack")
	}
	args := []string{"machine", "create", "--name", spec.Name, "--from", spec.Pack}
	for _, sizing := range []struct {
		flag  string
		value int
	}{
		{"--cpus", spec.CPUs},
		{"--mem", spec.MemoryMiB},
		{"--storage", spec.StorageGiB},
	} {
		if sizing.value > 0 {
			args = append(args, sizing.flag, strconv.Itoa(sizing.value))
		}
	}
	for _, label := range spec.Labels {
		args = append(args, "--label", label.Key+"="+label.Value)
	}
	for _, host := range spec.AllowHosts {
		args = append(args, "--allow-host-pattern", host)
	}
	for _, cred := range spec.Credentials {
		if len(cred.Hosts) == 0 {
			return nil, fmt.Errorf("credential %q binds no host", cred.Name)
		}
		args = append(args, "--credential", cred.Name+"="+cred.Env+"@"+strings.Join(cred.Hosts, ","))
	}
	if spec.SSHAgent {
		args = append(args, "--ssh-agent")
	}
	for _, v := range spec.Volumes {
		if err := checkMountPaths(v.Host, v.Guest); err != nil {
			return nil, err
		}
		mount := v.Host + ":" + v.Guest
		if v.ReadOnly {
			mount += ":ro"
		}
		args = append(args, "-v", mount)
	}
	for _, s := range spec.Sockets {
		if err := checkMountPaths(s.Host, s.Guest); err != nil {
			return nil, err
		}
		args = append(args, "--mount-socket", s.Host+":"+s.Guest)
	}
	return args, nil
}

func checkMountPaths(paths ...string) error {
	for _, p := range paths {
		if p == "" || strings.Contains(p, ":") {
			return fmt.Errorf("mount path %q is empty or contains ':'", p)
		}
	}
	return nil
}

// ExecOptions configures a command run in a machine.
type ExecOptions struct {
	// Workdir is the guest working directory; "" keeps the machine's.
	Workdir string

	// Env holds KEY=VALUE entries set in the guest command.
	Env []string

	// SecretEnv holds GUEST_VAR=HOST_VAR references smolvm resolves on
	// the host for this command.
	SecretEnv []string

	// Interactive keeps standard input open as a byte-exact pipe; the
	// guest command's output streams as it is written.
	Interactive bool

	// Stream, without Interactive, streams the guest command's output
	// as it is written instead of after the command exits; its
	// standard input is empty.
	Stream bool
}

// ExecArgs returns the arguments, without the program, of an invocation
// that runs argv in the machine name.
func ExecArgs(name string, opts ExecOptions, argv []string) []string {
	args := []string{"machine", "exec", "--name", name}
	if opts.Interactive {
		args = append(args, "-i")
	} else if opts.Stream {
		args = append(args, "--stream")
	}
	if opts.Workdir != "" {
		args = append(args, "-w", opts.Workdir)
	}
	for _, kv := range opts.Env {
		args = append(args, "-e", kv)
	}
	for _, ref := range opts.SecretEnv {
		args = append(args, "--secret-env", ref)
	}
	args = append(args, "--")
	return append(args, argv...)
}

// CLI drives one smolvm binary. Every invocation inherits this
// process's environment, which is where smolvm reads the values of the
// host variables credential bindings name. Safe for concurrent use.
type CLI struct {
	binary string
}

// NewCLI returns a client for binary; "" selects [DefaultBinary].
func NewCLI(binary string) *CLI {
	if binary == "" {
		binary = DefaultBinary
	}
	return &CLI{binary: binary}
}

// Binary returns the program the client runs.
func (c *CLI) Binary() string { return c.binary }

// run runs the binary with args and returns its standard output.
// subcommand and machine describe the invocation in a [CLIError].
func (c *CLI) run(ctx context.Context, subcommand []string, machine string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.binary, args...) //nolint:gosec // G204: the operator configures the binary; arguments are built by this package.
	stdout := &cappedWriter{max: maxStdoutBytes}
	stderr := procutil.NewTailBuffer(stderrTailBytes)
	result, err := procutil.RunCapture(ctx, cmd, procutil.DefaultStopGrace, procutil.CaptureParams{Stdout: stdout, Stderr: stderr})
	if err == nil {
		err = result.WaitErr
	}
	if err == nil && !result.OutputComplete {
		err = errOutputIncomplete
	}
	if err == nil && stdout.overflow {
		err = ErrOutputTooLarge
	}
	if err == nil {
		return stdout.buf.Bytes(), nil
	}
	cliErr := &CLIError{
		Args:     subcommand,
		Machine:  machine,
		ExitCode: -1,
		Stderr:   string(stderr.Bytes()),
		Err:      err,
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		cliErr.ExitCode = exitErr.ExitCode()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		cliErr.ExitCode = -1
		cliErr.Err = errors.Join(ctxErr, err)
	}
	return nil, cliErr
}

var versionPattern = regexp.MustCompile(`^smolvm v?(\d+)\.(\d+)\.(\d+)(\S*)`)

// Version runs the binary's version command and returns the version it
// prints, such as "1.26.1".
func (c *CLI) Version(ctx context.Context) (string, error) {
	out, err := c.run(ctx, []string{"--version"}, "", []string{"--version"})
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	m := versionPattern.FindStringSubmatch(line)
	if m == nil {
		return "", fmt.Errorf("smolvm --version printed %q, not a smolvm version", truncate(line, 120))
	}
	return m[1] + "." + m[2] + "." + m[3] + m[4], nil
}

// CheckVersion returns an error naming version and [SupportedVersion]
// when version is outside the supported range.
func CheckVersion(version string) error {
	m := versionPattern.FindStringSubmatch("smolvm " + version)
	if m != nil {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if major == supportedMajor && minor == supportedMinor {
			return nil
		}
	}
	return fmt.Errorf("smolvm version %s is not supported; supported: %s", version, SupportedVersion)
}

// List returns every machine smolvm knows.
func (c *CLI) List(ctx context.Context) ([]Machine, error) {
	out, err := c.run(ctx, []string{"machine", "ls"}, "", []string{"machine", "ls", "--json"})
	if err != nil {
		return nil, err
	}
	return parseMachines(out)
}

// Create creates the machine spec describes. It does not start it.
func (c *CLI) Create(ctx context.Context, spec CreateSpec) error {
	args, err := createArgs(spec)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, []string{"machine", "create"}, spec.Name, args)
	return err
}

// Start starts the machine name.
func (c *CLI) Start(ctx context.Context, name string) error {
	_, err := c.run(ctx, []string{"machine", "start"}, name, []string{"machine", "start", "--name", name})
	return err
}

// Stop stops the machine name. Stopping a machine that is not running
// succeeds.
func (c *CLI) Stop(ctx context.Context, name string) error {
	_, err := c.run(ctx, []string{"machine", "stop"}, name, []string{"machine", "stop", "--name", name})
	return err
}

// Delete deletes the machine name without asking for confirmation.
func (c *CLI) Delete(ctx context.Context, name string) error {
	_, err := c.run(ctx, []string{"machine", "delete"}, name, []string{"machine", "delete", "-f", "--name", name})
	return err
}

// Exec runs argv in the machine name and returns its standard output.
// It suits short commands with small output; a long-running command is
// built with [ExecArgs] instead.
func (c *CLI) Exec(ctx context.Context, name string, opts ExecOptions, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("smolvm machine exec needs a command")
	}
	return c.run(ctx, []string{"machine", "exec"}, name, ExecArgs(name, opts, argv))
}

// cappedWriter keeps up to max bytes and records whether more arrived.
type cappedWriter struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *cappedWriter) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room]) //nolint:errcheck // bytes.Buffer.Write never returns an error
		}
		return len(p), nil
	}
	b.buf.Write(p) //nolint:errcheck // bytes.Buffer.Write never returns an error
	return len(p), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
