package smolvm

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/environment"
)

func TestUnimplementedOperationsFail(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ws := environment.Workspace{Key: "VM-1", HostPath: t.TempDir()}
	env, err := New(environment.Params{})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if env.Kind() != "smolvm" {
		t.Errorf("Kind() = %q, want %q", env.Kind(), "smolvm")
	}

	for name, op := range map[string]func() error{
		"Release": func() error { return env.Release(ctx, ws) },
		"Remove":  func() error { return env.Remove(ctx, ws) },
		"Sweep":   func() error { return env.Sweep(ctx, environment.SweepParams{}) },
	} {
		if err := op(); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s() = %v, want ErrNotImplemented: a VM may exist", name, err)
		}
	}
}

func TestGuestPathIsTheGuestWorkspace(t *testing.T) {
	t.Parallel()

	env, err := New(environment.Params{})
	if err != nil {
		t.Fatal(err)
	}
	if got := env.GuestPath("/srv/workspaces/VM-1"); got != "/workspace" {
		t.Errorf("GuestPath() = %q, want /workspace", got)
	}
	ws := environment.Workspace{Key: "VM-1", HostPath: "/srv/workspaces/VM-1", GuestPath: env.GuestPath("/srv/workspaces/VM-1")}
	if got := ws.GuestPathOf("/srv/workspaces/VM-1/.sortie/mcp.json"); got != "/workspace/.sortie/mcp.json" {
		t.Errorf("GuestPathOf() = %q, want /workspace/.sortie/mcp.json", got)
	}
	namer, ok := env.(environment.Namer)
	if !ok || namer.InstanceName(ws) != "sortie-VM-1" {
		t.Errorf("InstanceName() = %v, want sortie-VM-1", namer)
	}
}

// smolvmAcceptedName is the name rule smolvm 1.26 enforces, measured:
// letters, digits, underscores, and single inner hyphens, starting with
// a letter or digit.
var smolvmAcceptedName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*(-[A-Za-z0-9_]+)*$`)

func TestVMName(t *testing.T) {
	t.Parallel()

	for key, want := range map[string]string{
		"VM-1":    "sortie-VM-1",
		"abc_123": "sortie-abc_123",
		"_x":      "sortie-_x",
	} {
		if got := VMName(key); got != want {
			t.Errorf("VMName(%q) = %q, want %q", key, got, want)
		}
	}

	long := strings.Repeat("A", 80)
	keys := []string{
		"a.b", "a_b", "a-b", "ABC--1", "x-", "-", "...", "-lead", "v1.2.3",
		long + "1", long + "2", strings.Repeat("a.", 60),
	}
	seen := map[string]string{}
	for _, key := range keys {
		name := VMName(key)
		if !smolvmAcceptedName.MatchString(name) || len(name) > 63 || !strings.HasPrefix(name, "sortie-") {
			t.Errorf("VMName(%q) = %q, want a name smolvm accepts, within 63 characters", key, name)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("VMName(%q) = VMName(%q) = %q, want distinct names", key, other, name)
		}
		seen[name] = key
		if VMName(key) != name {
			t.Errorf("VMName(%q) is not stable", key)
		}
	}
}
