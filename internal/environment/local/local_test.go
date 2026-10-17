package local

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/environment"
)

func testWorkspace(t *testing.T) environment.Workspace {
	t.Helper()
	host := t.TempDir()
	return environment.Workspace{Key: "ENV-1", HostPath: host, GuestPath: host, IssueID: "10001", Identifier: "ENV-1"}
}

func TestCommand(t *testing.T) {
	t.Parallel()

	ws := testWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws.HostPath, "sub", "dir"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tests := []struct {
		name    string
		spec    environment.CommandSpec
		wantDir string
		wantEnv []string
	}{
		{
			name:    "root directory and nil env inherit",
			spec:    environment.CommandSpec{Argv: []string{"sh", "-c", "true"}},
			wantDir: ws.HostPath,
			wantEnv: nil,
		},
		{
			name:    "relative directory and env verbatim",
			spec:    environment.CommandSpec{Argv: []string{"git", "status", "--porcelain"}, Dir: "sub/dir", Env: []string{"A=1", "SORTIE_WORKSPACE=" + ws.HostPath}},
			wantDir: filepath.Join(ws.HostPath, "sub", "dir"),
			wantEnv: []string{"A=1", "SORTIE_WORKSPACE=" + ws.HostPath},
		},
		{
			name:    "empty env stays empty",
			spec:    environment.CommandSpec{Argv: []string{"env"}, Env: []string{}, Stdin: true},
			wantDir: ws.HostPath,
			wantEnv: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd, err := Environment{}.Command(context.Background(), ws, tt.spec)
			if err != nil {
				t.Fatalf("Command() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(cmd.Args, tt.spec.Argv) {
				t.Errorf("cmd.Args = %q, want %q", cmd.Args, tt.spec.Argv)
			}
			if cmd.Dir != tt.wantDir {
				t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, tt.wantDir)
			}
			if !reflect.DeepEqual(cmd.Env, tt.wantEnv) || (cmd.Env == nil) != (tt.wantEnv == nil) {
				t.Errorf("cmd.Env = %#v, want %#v", cmd.Env, tt.wantEnv)
			}
			if cmd.Stdin != nil {
				t.Errorf("cmd.Stdin = %v, want nil: the caller connects standard input", cmd.Stdin)
			}
		})
	}
}

func TestCommandRuns(t *testing.T) {
	t.Parallel()

	ws := testWorkspace(t)
	cmd, err := Environment{}.Command(context.Background(), ws, environment.CommandSpec{Argv: []string{"go", "env", "GOROOT"}})
	if err != nil {
		t.Fatalf("Command() error = %v, want nil", err)
	}
	if out, err := cmd.Output(); err != nil || len(out) == 0 {
		t.Errorf("Output() = %q, %v, want the host command's output", out, err)
	}
}

func TestCommandBoundToContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd, err := Environment{}.Command(ctx, testWorkspace(t), environment.CommandSpec{Argv: []string{"go", "version"}})
	if err != nil {
		t.Fatalf("Command() error = %v, want nil", err)
	}
	if err := cmd.Run(); err == nil {
		t.Error("Run() with a cancelled context = nil, want an error")
	}
}

func TestCommandRejects(t *testing.T) {
	t.Parallel()

	ws := testWorkspace(t)
	for _, spec := range []environment.CommandSpec{
		{},
		{Argv: []string{}},
		{Argv: []string{"ls"}, Dir: ".."},
		{Argv: []string{"ls"}, Dir: "a/../../b"},
		{Argv: []string{"ls"}, Dir: filepath.Join(string(filepath.Separator), "etc")},
	} {
		if cmd, err := (Environment{}).Command(context.Background(), ws, spec); err == nil {
			t.Errorf("Command(%+v) = %v, nil, want an error", spec, cmd)
		}
	}
}

func TestCommandRefusesUnverifiedDirectory(t *testing.T) {
	t.Parallel()

	ws := testWorkspace(t)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(ws.HostPath, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.HostPath, "file"), nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, dir := range []string{"missing", "link", "file"} {
		cmd, err := Environment{}.Command(context.Background(), ws, environment.CommandSpec{Argv: []string{"ls"}, Dir: dir})
		if err == nil {
			t.Errorf("Command(Dir %q) = %v, nil, want an error: the working directory must be a real directory", dir, cmd)
		}
	}
	gone := ws
	gone.HostPath = filepath.Join(ws.HostPath, "removed")
	if cmd, err := (Environment{}).Command(context.Background(), gone, environment.CommandSpec{Argv: []string{"ls"}}); err == nil {
		t.Errorf("Command() in a missing workspace = %v, nil, want an error", cmd)
	}
}

func TestGuestPathIsIdentity(t *testing.T) {
	t.Parallel()

	for _, p := range []string{"", "/srv/ws/ENV-1", "/srv/ws/ENV-1/.sortie/mcp.json", "relative/path"} {
		if got := (Environment{}).GuestPath(p); got != p {
			t.Errorf("GuestPath(%q) = %q, want it unchanged", p, got)
		}
	}
}

func TestLifecycleNoOps(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ws := testWorkspace(t)
	env, err := New(environment.Params{})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if env.Kind() != "local" {
		t.Errorf("Kind() = %q, want %q", env.Kind(), "local")
	}

	res, err := env.Prepare(ctx, ws)
	if err != nil || res.CreatedNow {
		t.Errorf("Prepare() = %+v, %v, want CreatedNow false and nil", res, err)
	}
	if err := env.Release(ctx, ws); err != nil {
		t.Errorf("Release() = %v, want nil", err)
	}
	if err := env.Remove(ctx, ws); err != nil {
		t.Errorf("Remove() = %v, want nil", err)
	}
	if err := env.Sweep(ctx, environment.SweepParams{WorkspaceRoot: filepath.Dir(ws.HostPath)}); err != nil {
		t.Errorf("Sweep() = %v, want nil", err)
	}
	if entries, err := filepath.Glob(filepath.Join(ws.HostPath, "*")); err != nil || len(entries) != 0 {
		t.Errorf("workspace entries after the lifecycle = %q, %v, want none: local changes nothing", entries, err)
	}
}

func TestRegistered(t *testing.T) {
	t.Parallel()

	ctor, err := environment.Registry.Get("local")
	if err != nil {
		t.Fatalf("Registry.Get(local) error = %v", err)
	}
	env, err := ctor(environment.Params{})
	if err != nil || env.Kind() != "local" {
		t.Errorf("constructor() = %v, %v, want the local kind", env, err)
	}
	if meta, ok := environment.Registry.Meta("local"); !ok || meta.CheckHost != nil {
		t.Errorf("Registry.Meta(local) = %+v, %v, want registered with no host check", meta, ok)
	}
}
