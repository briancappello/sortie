package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

func TestAlwaysLoadToolServer(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	sortieDir := filepath.Join(workspace, workspacekit.SortieDir)
	if err := os.Mkdir(sortieDir, 0o700); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(sortieDir, "mcp.json")
	const config = `{"mcpServers":{"sortie-tools":{"type":"stdio","command":"sortie"},"operator":{"type":"http","url":"https://mcp.example.com"}}}`
	if err := os.WriteFile(generated, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	path, err := alwaysLoadToolServer(generated)
	if err != nil {
		t.Fatalf("alwaysLoadToolServer() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.MCPServers["sortie-tools"]["alwaysLoad"]; got != true {
		t.Errorf("sortie-tools alwaysLoad = %v, want true", got)
	}
	if got := doc.MCPServers["sortie-tools"]["command"]; got != "sortie" {
		t.Errorf("sortie-tools command = %v, want the generated entry preserved", got)
	}
	if _, set := doc.MCPServers["operator"]["alwaysLoad"]; set {
		t.Error("operator server carries alwaysLoad, want only sortie-tools changed")
	}
}
