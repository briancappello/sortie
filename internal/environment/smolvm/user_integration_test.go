//go:build unix

package smolvm

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// userPack returns the pack and the non-root user it defines that the
// user test creates its VM from: SORTIE_SMOLVM_TEST_USER_PACK, and
// SORTIE_SMOLVM_TEST_USER or builder.
func userPack(t *testing.T) (pack, user string) {
	t.Helper()
	pack = os.Getenv("SORTIE_SMOLVM_TEST_USER_PACK")
	if pack == "" {
		t.Fatal("SORTIE_SMOLVM_TEST_USER_PACK is unset; set it to a pack whose image defines a non-root user (SORTIE_SMOLVM_TEST_USER, default builder)")
	}
	user = os.Getenv("SORTIE_SMOLVM_TEST_USER")
	if user == "" {
		user = "builder"
	}
	return pack, user
}

// passwdHomeScript prints the home directory /etc/passwd gives the user
// the script runs as.
const passwdHomeScript = `awk -F: -v u="$(id -un)" '$1 == u { print $6 }' /etc/passwd`

// Not parallel: it sets the git configuration variables in the process
// environment.
func TestIntegration_UserRunsEveryCommand(t *testing.T) {
	requireSmolvm(t)
	pack, user := userPack(t)
	ie := newIntegrationEnvWith(t, func(cfg *config.SmolvmConfig) {
		cfg.Pack = pack
		cfg.User = user
	})
	up := newRepoFixture(t)
	syncer := newSyncer(t, up.dir)
	key := ie.newKey("SMOLUSER")
	ctx := liveContext(t)

	// Seeding runs before after_create, as the machine's user.
	res, err := workspace.Prepare(ctx, workspace.PrepareParams{
		Root:          ie.root,
		Identifier:    key,
		IssueID:       "10001",
		AfterCreate:   `{ id -un; id -u; printf '%s\n' "$HOME"; ` + passwdHomeScript + `; stat -c %U /workspace/.git; } > /tmp/hook-user`,
		HookTimeoutMS: 120_000,
		Environment:   ie.env,
		Repository:    syncer,
	})
	if err != nil {
		t.Fatalf("workspace.Prepare() error = %v", err)
	}
	hook, err := ie.guest(ctx, key, "cat /tmp/hook-user")
	if err != nil {
		t.Fatalf("guest cat /tmp/hook-user: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(hook), "\n")
	if len(lines) != 5 || lines[0] != user || lines[1] == "0" || lines[2] != lines[3] || lines[2] == "" || lines[2] == "/root" || lines[4] != user {
		t.Errorf("after_create user, uid, HOME, passwd home and clone owner = %q; want %s, not uid 0, %s's home, and a clone %s owns", lines, user, user, user)
	}

	// The agent's shell.
	hostArgv, err := ie.env.LaunchArgv(environment.Workspace{Key: key, HostPath: res.Path, GuestPath: GuestWorkspace, Identifier: key},
		domain.CarrierLaunch{Argv: []string{"sh", "-c", `id -un; printf '%s\n' "$HOME"; ` + passwdHomeScript}})
	if err != nil {
		t.Fatalf("LaunchArgv() error = %v", err)
	}
	cmd := exec.CommandContext(ctx, hostArgv[0], hostArgv[1:]...) //nolint:gosec // G204: the test's own smolvm launch
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	agent := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil || len(agent) != 3 || agent[0] != user || agent[1] != agent[2] || agent[1] == "" || agent[1] == "/root" {
		t.Errorf("agent's id -un, HOME and passwd home = %q, %v; want %s and %s's home", agent, err, user, user)
	}

	// Export runs as the machine's user too.
	head, err := ie.guest(ctx, key, `cd /workspace && echo work > agent.txt && git add agent.txt &&
git -c user.name=Agent -c user.email=agent@example.com commit -q -m work && git rev-parse HEAD`)
	if err != nil {
		t.Fatalf("guest commit: %v", err)
	}
	ws := environment.Workspace{Key: res.Key, HostPath: res.Path, GuestPath: GuestWorkspace, Identifier: key}
	got, err := syncer.Export(ctx, ie.env, ws)
	if err != nil || !got.Pushed || got.Commit != strings.TrimSpace(head) {
		t.Errorf("Export() = %+v, %v; want %s pushed", got, err, strings.TrimSpace(head))
	}
	if out, err := ie.guest(ctx, key, "test ! -e "+GuestUserTransferDir+"/out.bundle && test ! -e "+GuestUserTransferDir+"/in.bundle"); err != nil {
		t.Errorf("guest bundles after export = %q, %v; want none left", out, err)
	}
}
