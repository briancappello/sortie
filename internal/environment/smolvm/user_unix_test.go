//go:build unix

package smolvm

import (
	"context"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/gitsync"
)

// A workflow that sets user creates each VM with it as the machine's
// user, and with the argv it would have without user otherwise.
func TestPrepare_UserIsTheMachinesUser(t *testing.T) {
	t.Parallel()

	for _, user := range []string{"builder", "1000", "1000:1000"} {
		t.Run(user, func(t *testing.T) {
			t.Parallel()
			f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
				parsed := workflowSmolvmConfig(t, cfg.Pack, map[string]any{"user": user, "allow_hosts": []any{"ghcr.io"}})
				parsed.Binary = cfg.Binary
				*cfg = parsed
			})
			on := createArgv(t, f)
			if n := countArg(on, "--user"); n != 1 {
				t.Fatalf("create argv = %q, want --user once", on)
			}
			if i := slices.Index(on, "--user"); i+1 >= len(on) || on[i+1] != user {
				t.Errorf("create argv = %q, want --user %s", on, user)
			}
			assertOnlyGrantedResources(t, on)

			// The same workspace without user: the argv it would be
			// created with.
			f.env.cfg.User = ""
			paths, err := f.env.preparePaths(f.ws)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := f.env.createSpec(VMName(f.ws.Key), f.ws, paths)
			if err != nil {
				t.Fatal(err)
			}
			off, err := createArgs(spec)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(off, "--user") {
				t.Errorf("create argv without user = %q, want no --user", off)
			}
			i := slices.Index(on, "--user")
			if without := slices.Delete(slices.Clone(on), i, i+2); !slices.Equal(without, off) {
				t.Errorf("create argv with user less --user %s = %q, want the argv without it %q", user, without, off)
			}
		})
	}
}

// The machine's user carries every command: no command Sortie runs in
// the VM names a user of its own, so each runs as the machine's.
func TestCommandsRunAsTheMachinesUser(t *testing.T) {
	t.Parallel()

	argvs := func(user string) ([]string, []string) {
		t.Helper()
		f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) { cfg.User = user })
		cmd, err := f.env.Command(context.Background(), f.ws, environment.CommandSpec{Argv: []string{"id", "-un"}})
		if err != nil {
			t.Fatalf("Command() error = %v", err)
		}
		launch, err := f.env.LaunchArgv(f.ws, domain.CarrierLaunch{Argv: []string{"agent"}})
		if err != nil {
			t.Fatalf("LaunchArgv() error = %v", err)
		}
		return cmd.Args[1:], launch[1:]
	}
	cmdOff, launchOff := argvs("")
	cmdOn, launchOn := argvs("builder")
	if !slices.Equal(cmdOn, cmdOff) || !slices.Equal(launchOn, launchOff) {
		t.Errorf("with user: command %q, launch %q; want the argvs without it, %q and %q", cmdOn, launchOn, cmdOff, launchOff)
	}
	for _, argv := range [][]string{cmdOn, launchOn} {
		if slices.Contains(argv, "--user") {
			t.Errorf("exec argv = %q, want no user of its own", argv)
		}
	}
}

// A VM whose user is set stages repository bundles where that user can
// write; any other keeps Sortie's own directory.
func TestGuestTransferDir(t *testing.T) {
	t.Parallel()

	withUser := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) { cfg.User = "builder" }).env
	if got := withUser.GuestTransferDir(); got != GuestUserTransferDir {
		t.Errorf("GuestTransferDir() with user = %q, want %q", got, GuestUserTransferDir)
	}
	if got := environment.GuestTransferDir(withUser, gitsync.GuestGitDir); got != GuestUserTransferDir {
		t.Errorf("environment.GuestTransferDir() with user = %q, want %q", got, GuestUserTransferDir)
	}
	without := newPrepareFixture(t, fakeState{}, nil).env
	if got := without.GuestTransferDir(); got != "" {
		t.Errorf("GuestTransferDir() without user = %q, want none", got)
	}
	if got := environment.GuestTransferDir(without, gitsync.GuestGitDir); got != gitsync.GuestGitDir {
		t.Errorf("environment.GuestTransferDir() without user = %q, want %q", got, gitsync.GuestGitDir)
	}
}
