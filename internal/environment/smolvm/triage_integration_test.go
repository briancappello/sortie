//go:build unix

package smolvm

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// The triage command runs in a stopped VM, which is resumed for it, with
// its input and result files on the guest's tmpfs and none in either
// workspace. /run being a tmpfs is checked from inside the guest.
func TestIntegration_ReactionTriageFilesInTheGuest(t *testing.T) {
	ie := newIntegrationEnv(t)
	key := ie.newKey("SMOLTRIAGE")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := workspace.Prepare(ctx, workspace.PrepareParams{
		Root: ie.root, Identifier: key, IssueID: "10001", HookTimeoutMS: 120_000, Environment: ie.env,
	})
	if err != nil {
		t.Fatalf("workspace.Prepare() error = %v", err)
	}
	if err := ie.env.Release(ctx, environment.Workspace{Key: key, HostPath: res.Path, GuestPath: GuestWorkspace}); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	script := `set -e
case "$SORTIE_REACTION_INPUT" in /run/sortie/triage/*/input.json) ;; *) exit 9 ;; esac
grep -q '"workspace":"/workspace"' "$SORTIE_REACTION_INPUT"
awk '$2 == "/run" { print $3 }' /proc/mounts | grep -qx tmpfs
test "$(stat -c %a "$(dirname "$SORTIE_REACTION_INPUT")")" = 700
echo '{"disposition":"escalate"}' > "$SORTIE_REACTION_RESULT"`
	outcome := orchestrator.RunReactionTriage(ctx, config.ReactionTriageConfig{Script: script, TimeoutMS: 120_000},
		orchestrator.ReactionTriageRequest{
			Kind: "ci", WorkspaceRoot: ie.root, IssueID: "10001", Identifier: key,
			Fingerprint: "fp-smolvm", Subject: map[string]any{"head_sha": "fp-smolvm"},
			Environment: ie.env,
		}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if outcome.Disposition != orchestrator.TriageEscalate || outcome.Fallback != "" {
		t.Fatalf("outcome = %+v, want escalate read from the guest", outcome)
	}

	left, err := ie.guest(ctx, key, "ls -A /run/sortie/triage; ls -A /workspace")
	if err != nil || strings.TrimSpace(left) != ".sortie" {
		t.Errorf("guest leftovers = %q, %v, want the run directory removed and only .sortie in /workspace", left, err)
	}
	entries, err := os.ReadDir(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".sortie" {
			t.Errorf("host workspace holds %q, want nothing but .sortie", e.Name())
		}
	}
}
