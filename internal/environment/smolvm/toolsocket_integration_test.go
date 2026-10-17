//go:build unix

package smolvm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/toolsocket"
)

// mcpStatusCall is a scripted MCP client: the handshake, then one call
// of sortie_status.
const mcpStatusCall = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"gated-test","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sortie_status","arguments":{}}}
`

// buildStaticSortie builds the Sortie binary as a release does, static,
// for the VM's binary directory and the host tool server alike.
func buildStaticSortie(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "sortie")
	cmd := exec.Command("go", "build", "-o", out, "../../../cmd/sortie")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	return out
}

// statusText returns the text of the sortie_status call's result in
// the relay's output.
func statusText(t *testing.T, stdout []byte) string {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var msg struct {
			ID     int `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.ID == 2 && len(msg.Result.Content) > 0 {
			return msg.Result.Content[0].Text
		}
	}
	t.Fatalf("no sortie_status result in the relay's output:\n%s", stdout)
	return ""
}

func TestIntegration_GuestCallsSortieToolsOnTheHost(t *testing.T) {
	ie := newIntegrationEnv(t)
	sortie := buildStaticSortie(t)
	ie.env.sortieBinary = sortie
	key := ie.newKey("SMOLTOOLS")
	host := filepath.Join(ie.root, key)
	if err := os.Mkdir(host, 0o750); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(ie.root, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\n---\nprompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := environment.Workspace{Key: key, HostPath: host, GuestPath: GuestWorkspace, IssueID: "id-" + key, Identifier: key}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := ie.env.Prepare(ctx, ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	// The worker writes the session's state on the host; the tool server
	// reads it there.
	state := `{"turn_number":4,"max_turns":17,"started_at":"2026-10-12T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(host, ".sortie", "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	socket, err := ie.env.ToolSocketPath(ws)
	if err != nil {
		t.Fatalf("ToolSocketPath() error = %v", err)
	}
	spec := toolsocket.Spec{
		Argv: []string{sortie, "mcp-server", "--workflow", workflow},
		Env: []string{
			"PATH=" + os.Getenv("PATH"),
			"SORTIE_WORKSPACE=" + host,
			"SORTIE_ISSUE_ID=" + ws.IssueID,
			"SORTIE_ISSUE_IDENTIFIER=" + ws.Identifier,
		},
	}
	serve := func() *toolsocket.Registry {
		t.Helper()
		reg := toolsocket.NewRegistry(ie.env.logger)
		t.Cleanup(reg.Close)
		if err := reg.Serve(key, socket, spec); err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
		return reg
	}
	relay := func() (stdout, stderr string, err error) {
		t.Helper()
		cmd, err := ie.env.Command(ctx, ws, environment.CommandSpec{
			Argv:  []string{ie.env.GuestSortieBinaryPath(), "mcp-relay", ie.env.GuestToolSocketPath()},
			Stdin: true,
		})
		if err != nil {
			t.Fatalf("Command() error = %v", err)
		}
		var out, errOut bytes.Buffer
		cmd.Stdin = strings.NewReader(mcpStatusCall)
		cmd.Stdout, cmd.Stderr = &out, &errOut
		runErr := cmd.Run()
		return out.String(), errOut.String(), runErr
	}
	assertStatus := func(stage string) {
		t.Helper()
		stdout, stderr, err := relay()
		if err != nil {
			t.Fatalf("%s: relay in the guest failed: %v\nstderr: %s", stage, err, stderr)
		}
		text := statusText(t, []byte(stdout))
		if !strings.Contains(text, `"turn_number":4`) || !strings.Contains(text, `"max_turns":17`) {
			t.Errorf("%s: sortie_status = %s, want this workspace's state", stage, text)
		}
	}

	reg := serve()
	assertStatus("first listener")

	// No tool server answers once the listener is gone: the relay fails
	// fast instead of waiting.
	reg.Stop(key)
	start := time.Now()
	_, stderr, err := relay()
	if err == nil {
		t.Fatal("relay succeeded with no listener, want a non-zero exit")
	}
	if !strings.Contains(stderr, "tool server unavailable") {
		t.Errorf("relay stderr = %q, want it to contain %q", stderr, "tool server unavailable")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("relay took %v to fail without a listener", elapsed)
	}

	// A new listener at the same path, as after a Sortie restart, serves
	// the running VM.
	serve()
	assertStatus("listener after restart")
}
