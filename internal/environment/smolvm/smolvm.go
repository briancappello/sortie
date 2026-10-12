// Package smolvm registers the smolvm execution environment kind, which
// runs each issue workspace in its own smolvm microVM. It registers
// itself in [environment.Registry] from init.
//
// The package drives smolvm through its command line only, with [CLI];
// it is the one place that knows smolvm's flags, JSON output, and exit
// codes. The kind's host check, [CheckHost], refuses a smolvm release
// outside [SupportedVersion].
//
// [Environment.Prepare] creates and starts the workspace's VM,
// [Environment.AfterCreate] links the guest workspace's .sortie to the
// control directory it shares with the host, and [Environment.Command]
// and [Environment.LaunchArgv] run hooks, Sortie's own commands, and the
// agent in it. [Environment.Release] stops the VM when the worker exits,
// [Environment.Resume] starts it again for the before_remove hook,
// [Environment.Remove] deletes it with its workspace, and
// [Environment.Sweep] deletes the VMs whose workspace directory is gone.
//
// The package touches only a VM it labeled as managed, for the
// workspace key the VM's name derives from, and for this workflow; any
// other machine is left alone.
package smolvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspace"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// Paths inside the VM.
const (
	// GuestWorkspace is the workspace directory on the VM's own disk.
	GuestWorkspace = "/workspace"

	// GuestControlDir is where the host workspace's .sortie directory
	// is mounted, read-write.
	GuestControlDir = "/run/sortie/control"

	// GuestBinDir is where the directory holding only the Sortie binary
	// is mounted, read-only.
	GuestBinDir = "/opt/sortie"

	// GuestToolSocket is where the issue's tool-server socket is
	// mounted.
	GuestToolSocket = "/run/sortie/tools.sock"
)

// Labels Sortie puts on every VM it creates.
const (
	LabelManaged   = "sortie.managed"
	LabelWorkspace = "sortie.workspace"
	LabelPack      = "sortie.pack"
	LabelWorkflow  = "sortie.workflow"
)

// Layout of the environment's directories under the state directory:
// env/<key> holds each workspace's tool socket, and env-bin, apart from
// every per-workspace directory, holds the Sortie binary the VMs mount.
const (
	stateEnvDir      = "env"
	stateBinDir      = "env-bin"
	toolSocketName   = "tools.sock"
	recordMaxBytes   = 4096
	maxUnixSocketLen = 107 // sun_path holds 108 bytes, one of them the terminating NUL
)

// VM names. smolvm accepts [A-Za-z0-9_-], starting with a letter or
// digit, without a trailing or doubled hyphen, and up to 128
// characters, but a machine whose name exceeds 117 characters cannot
// start. Names stay within a DNS label's 63 characters.
const (
	vmNamePrefix  = "sortie-"
	maxVMNameLen  = 63
	nameHashChars = 10
)

var plainVMNameKey = regexp.MustCompile(`^[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*$`)

func init() {
	environment.Registry.RegisterWithMeta(config.EnvironmentKindSmolvm, New, environment.Meta{
		CheckHost:       CheckHost,
		SweepUnselected: SweepUnselected,
	})
}

// Environment is the smolvm kind. It is safe for concurrent use across
// different workspaces.
type Environment struct {
	cfg          config.SmolvmConfig
	cli          *CLI
	stateDir     string
	workflowHash string
	logger       *slog.Logger

	packMu   sync.Mutex
	packSums map[string]packSum
}

// packSum is a pack file's SHA-256 together with the size and
// modification time it was computed for.
type packSum struct {
	size    int64
	modTime time.Time
	sum     string
}

var (
	_ environment.Environment = (*Environment)(nil)
	_ environment.Carrier     = (*Environment)(nil)
	_ environment.Namer       = (*Environment)(nil)

	_ environment.AfterCreateFinisher = (*Environment)(nil)
	_ environment.Resumer             = (*Environment)(nil)
	_ environment.EnvForwarder        = (*Environment)(nil)
)

// New returns the smolvm environment for params. It succeeds without a
// pack, so that an environment built only to remove VMs of a workflow
// that no longer selects the kind can be built; [Environment.Prepare]
// then fails.
func New(params environment.Params) (environment.Environment, error) {
	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}
	env := &Environment{
		cfg:      params.Config.Smolvm,
		cli:      NewCLI(params.Config.Smolvm.Binary),
		stateDir: params.StateDir,
		logger:   logger,
		packSums: map[string]packSum{},
	}
	if params.WorkflowPath != "" {
		abs, err := filepath.Abs(params.WorkflowPath)
		if err != nil {
			return nil, fmt.Errorf("resolve workflow path: %w", err)
		}
		env.workflowHash = shortHash(abs)
	}
	return env, nil
}

// Kind returns [config.EnvironmentKindSmolvm].
func (*Environment) Kind() string { return config.EnvironmentKindSmolvm }

// GuestPath returns [GuestWorkspace]. Every host path handed to it names
// a host workspace directory, and every workspace's guest directory is
// [GuestWorkspace]. A path inside a workspace maps through
// [environment.Workspace.GuestPathOf].
func (*Environment) GuestPath(string) string { return GuestWorkspace }

// ForwardsEnv reports whether the host variable name reaches the VM: as
// the placeholder of a credential binding, or in plain text through
// pass_env.
func (e *Environment) ForwardsEnv(name string) bool {
	for _, cred := range e.cfg.Credentials {
		if cred.Env == name {
			return true
		}
	}
	return slices.Contains(e.cfg.PassEnv, name)
}

// InstanceName returns the name of the VM that runs ws.
func (*Environment) InstanceName(ws environment.Workspace) string { return VMName(ws.Key) }

// VMName returns the name of the VM for the workspace key: "sortie-"
// followed by the key. A key smolvm would refuse in a name, or one that
// makes the name too long, is rewritten to the characters smolvm
// accepts, truncated, and followed by a short hash of the whole key, so
// distinct keys keep distinct names.
func VMName(key string) string {
	if plainVMNameKey.MatchString(key) && len(vmNamePrefix)+len(key) <= maxVMNameLen {
		return vmNamePrefix + key
	}
	var b strings.Builder
	lastHyphen := true // drops leading hyphens
	for _, r := range key {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	suffix := shortHash(key)[:nameHashChars]
	base := b.String()
	if room := maxVMNameLen - len(vmNamePrefix) - len(suffix) - 1; len(base) > room {
		base = base[:room]
	}
	base = strings.TrimRight(base, "-")
	if base == "" {
		return vmNamePrefix + suffix
	}
	return vmNamePrefix + base + "-" + suffix
}

// shortHash returns the hex SHA-256 of s, cut to 12 characters.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// Prepare makes the workspace's VM ready to run commands.
//
// It creates the host workspace's .sortie directory and the state
// directories the VM's mounts name, then looks the VM up. Without a VM,
// it creates one from the pack and starts it, and reports CreatedNow. A
// stopped VM is started; a running one is left as it is. A VM is reused
// only when the workspace's environment record names it: a VM that
// predates a fresh host workspace directory is deleted and created
// again. The result's image digest is the pack SHA-256 the VM's label
// records. A VM with the workspace's name that Sortie did not label as
// managed, for the workspace key, and for this workflow is never
// touched, and fails preparation. A smolvm
// failure fails preparation with a [*CLIError].
func (e *Environment) Prepare(ctx context.Context, ws environment.Workspace) (environment.PrepareResult, error) {
	paths, err := e.preparePaths(ws)
	if err != nil {
		return environment.PrepareResult{}, err
	}
	name := VMName(ws.Key)

	machines, err := e.cli.List(ctx)
	if err != nil {
		return environment.PrepareResult{}, err
	}
	var existing *Machine
	for i := range machines {
		if machines[i].Name == name {
			existing = &machines[i]
			break
		}
	}

	if existing != nil {
		// Workspace keys repeat across workflows, so a VM of the same
		// name may be another workflow's: only this workflow's own VM is
		// started, reused, or replaced.
		if !e.owns(*existing, ws.Key) {
			return environment.PrepareResult{}, fmt.Errorf(
				"smolvm machine %q exists but is not this workflow's VM for workspace %q "+
					"(it belongs to another workflow or is not managed by Sortie); remove or rename it", name, ws.Key)
		}
		if recordNames(ws.HostPath, name) {
			// The label names the pack the VM was created from, which a
			// rebuild of the pack file since does not change.
			reused := environment.PrepareResult{ImageDigest: existing.Labels[LabelPack]}
			if existing.Running() {
				return reused, nil
			}
			e.warnUnsetCredentials(ctx, name)
			if err := e.cli.Start(ctx, name); err != nil {
				return environment.PrepareResult{}, err
			}
			return reused, nil
		}
		e.logger.InfoContext(ctx, "deleting a VM that predates its workspace directory",
			slog.String("vm", name), slog.String("workspace", ws.HostPath))
		if err := e.cli.Delete(ctx, name); err != nil && !IsNotFound(err) {
			return environment.PrepareResult{}, err
		}
	}

	spec, err := e.createSpec(name, ws, paths)
	if err != nil {
		return environment.PrepareResult{}, err
	}
	if err := e.cli.Create(ctx, spec); err != nil {
		return environment.PrepareResult{}, err
	}
	e.warnUnsetCredentials(ctx, name)
	if err := e.cli.Start(ctx, name); err != nil {
		return environment.PrepareResult{}, err
	}
	e.logger.InfoContext(ctx, "created VM", slog.String("vm", name), slog.String("workspace", ws.HostPath))
	return environment.PrepareResult{CreatedNow: true, ImageDigest: labelValue(spec.Labels, LabelPack)}, nil
}

// labelValue returns the value of the label key among labels, or "".
func labelValue(labels []Label, key string) string {
	for _, l := range labels {
		if l.Key == key {
			return l.Value
		}
	}
	return ""
}

// hostPaths holds the host side of a VM's mounts.
type hostPaths struct {
	control string
	binDir  string
	socket  string
}

// preparePaths checks the configuration and the workspace, and creates
// the host directories the VM's mounts name.
func (e *Environment) preparePaths(ws environment.Workspace) (hostPaths, error) {
	if e.cfg.Pack == "" {
		return hostPaths{}, &config.ConfigError{Field: "environment.smolvm.pack", Message: "is required to create a smolvm VM"}
	}
	if e.stateDir == "" || e.workflowHash == "" {
		return hostPaths{}, errors.New("smolvm environment: a state directory and a workflow path are required to prepare a VM")
	}
	if ws.Key == "" || ws.Key == "." || !filepath.IsLocal(ws.Key) || strings.ContainsAny(ws.Key, `/\`) {
		return hostPaths{}, fmt.Errorf("smolvm environment: workspace key %q cannot name a state directory", ws.Key)
	}
	if !filepath.IsAbs(ws.HostPath) {
		return hostPaths{}, fmt.Errorf("smolvm environment: workspace path %q is not absolute", ws.HostPath)
	}
	stateDir, err := filepath.Abs(e.stateDir)
	if err != nil {
		return hostPaths{}, fmt.Errorf("resolve state directory: %w", err)
	}
	envDir := filepath.Join(stateDir, stateEnvDir)
	paths := hostPaths{
		control: filepath.Join(ws.HostPath, workspacekit.SortieDir),
		binDir:  filepath.Join(stateDir, stateBinDir),
		socket:  filepath.Join(envDir, ws.Key, toolSocketName),
	}
	if len(paths.socket) > maxUnixSocketLen {
		return hostPaths{}, fmt.Errorf("smolvm environment: tool socket path %q is longer than a Unix socket path may be (%d bytes)",
			paths.socket, maxUnixSocketLen)
	}

	dir, err := workspacekit.OpenSortieDir(ws.HostPath, true)
	if err != nil {
		return hostPaths{}, fmt.Errorf("create %s: %w", paths.control, err)
	}
	_ = dir.Close()
	if err := os.MkdirAll(paths.binDir, 0o750); err != nil {
		return hostPaths{}, fmt.Errorf("create %s: %w", paths.binDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.socket), 0o700); err != nil {
		return hostPaths{}, fmt.Errorf("create %s: %w", filepath.Dir(paths.socket), err)
	}
	return paths, nil
}

// recordNames reports whether the environment record of the workspace at
// hostPath names the smolvm VM name. A missing, unreadable, or malformed
// record names none.
func recordNames(hostPath, name string) bool {
	data, err := workspacekit.ReadSortieFile(hostPath, workspace.EnvironmentRecordFile, recordMaxBytes)
	if err != nil {
		return false
	}
	var rec workspace.EnvironmentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return false
	}
	return rec.Kind == config.EnvironmentKindSmolvm && rec.Name == name
}

// createSpec describes the VM for ws: the configured pack and sizing,
// the labels, the egress allowlist and credential bindings, and exactly
// three host resources: the control directory, the binary directory
// read-only, and the tool socket.
func (e *Environment) createSpec(name string, ws environment.Workspace, paths hostPaths) (CreateSpec, error) {
	packSHA, err := e.packSHA256(e.cfg.Pack)
	if err != nil {
		return CreateSpec{}, err
	}
	spec := CreateSpec{
		Name:       name,
		Pack:       e.cfg.Pack,
		CPUs:       e.cfg.CPUs,
		MemoryMiB:  e.cfg.MemoryMiB,
		StorageGiB: e.cfg.StorageGiB,
		Labels: []Label{
			{LabelManaged, "1"},
			{LabelWorkspace, ws.Key},
			{LabelPack, packSHA},
			{LabelWorkflow, e.workflowHash},
		},
		SSHAgent: e.cfg.SSHAgent,
		Volumes: []Volume{
			{Host: paths.control, Guest: GuestControlDir},
			{Host: paths.binDir, Guest: GuestBinDir, ReadOnly: true},
		},
		Sockets: []SocketMount{{Host: paths.socket, Guest: GuestToolSocket}},
	}
	// A credential binding alone gives the VM unrestricted egress, so
	// its hosts always join the allowlist.
	seen := map[string]bool{}
	addHost := func(h string) {
		if !seen[h] {
			seen[h] = true
			spec.AllowHosts = append(spec.AllowHosts, h)
		}
	}
	for _, h := range e.cfg.AllowHosts {
		addHost(h)
	}
	for _, cred := range e.cfg.Credentials {
		for _, h := range cred.Hosts {
			addHost(h)
		}
		spec.Credentials = append(spec.Credentials, CredentialBinding{Name: cred.Name, Env: cred.Env, Hosts: cred.Hosts})
	}
	return spec, nil
}

// packSHA256 returns the hex SHA-256 of the pack file at path, reusing
// the last result while the file's size and modification time are
// unchanged.
func (e *Environment) packSHA256(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", &config.ConfigError{Field: "environment.smolvm.pack", Message: fmt.Sprintf("pack %q is not readable: %v", path, err)}
	}
	if !info.Mode().IsRegular() {
		return "", &config.ConfigError{Field: "environment.smolvm.pack", Message: fmt.Sprintf("pack %q is not a regular file", path)}
	}
	e.packMu.Lock()
	defer e.packMu.Unlock()
	if cached, ok := e.packSums[path]; ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		return cached.sum, nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the pack file.
	if err != nil {
		return "", &config.ConfigError{Field: "environment.smolvm.pack", Message: fmt.Sprintf("pack %q is not readable: %v", path, err)}
	}
	defer f.Close() //nolint:errcheck // read-only file
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash pack %q: %w", path, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	e.packSums[path] = packSum{size: info.Size(), modTime: info.ModTime(), sum: sum}
	return sum, nil
}

// warnUnsetCredentials logs each credential binding whose host variable
// is not set. smolvm reads the value when the VM starts; without it, the
// VM starts and every request to the binding's hosts fails.
func (e *Environment) warnUnsetCredentials(ctx context.Context, name string) {
	for _, cred := range e.cfg.Credentials {
		if _, ok := os.LookupEnv(cred.Env); !ok {
			e.logger.WarnContext(ctx, "credential variable is not set; requests to its hosts from the VM will fail",
				slog.String("vm", name), slog.String("credential", cred.Name), slog.String("variable", cred.Env))
		}
	}
}
