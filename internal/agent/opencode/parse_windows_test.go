//go:build windows

package opencode

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const exportHeldDescendantScenario = "opencode.export-held-descendant"

type exportHeldDescendantParams struct {
	ChildPath    string
	ChildPIDPath string
	Body         string
}

func runExportHeldDescendant(args []string, p exportHeldDescendantParams) int {
	if len(args) < 2 || args[0] != "session" || args[1] != "export" {
		return 0
	}
	if err := startOpencodeWinHeldDescendant(p.ChildPath, p.ChildPIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "export held descendant: %v\n", err)
		return 2
	}
	fmt.Print(p.Body)
	return 0
}

func startOpencodeWinHeldDescendant(childPath, childPIDPath string) error {
	cmd := exec.Command(childPath) //nolint:gosec // fake runtime path this scenario was handed
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return os.WriteFile(childPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
}

func init() {
	fakeScenarios[exportHeldDescendantScenario] = agenttest.Typed(runExportHeldDescendant)
}

func TestQueryExportUsage_HeldDescendantHoldingOutput(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	binPath := agenttest.FakeRuntime(t, dir, "opencode", exportHeldDescendantScenario, exportHeldDescendantParams{
		ChildPath:    descendantPath,
		ChildPIDPath: pidPath,
		Body:         string(loadFixture(t, "export_usage.json")),
	})
	state := &sessionState{
		target: agentcore.LaunchTarget{
			Command:       binPath,
			WorkspacePath: dir,
		},
		sessionID:  "ses_abc123",
		baseLogger: slog.Default(),
	}

	type outcome struct{ usage exportUsage }
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		done <- outcome{queryExportUsage(context.Background(), state, 0)}
	}()

	oc := waittest.Receive(t, done, "queryExportUsage return despite the held descendant")
	if elapsed := time.Since(start); elapsed > procutil.DefaultDrainGrace {
		t.Errorf("queryExportUsage() took %v, want under the default drain grace %v", elapsed, procutil.DefaultDrainGrace)
	}
	if oc.usage.Usage.InputTokens != 1750 {
		t.Errorf("InputTokens = %d, want 1750", oc.usage.Usage.InputTokens)
	}
	if oc.usage.Usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", oc.usage.Usage.OutputTokens)
	}

	waittest.ProcessGone(t, waittest.PIDFile(t, pidPath))
}
