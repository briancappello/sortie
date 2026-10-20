package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

const (
	toolServerName       = "sortie-tools"
	alwaysLoadConfigName = "claude-mcp.json"
)

// alwaysLoadToolServer writes a copy of the generated MCP config whose
// sortie-tools entry carries "alwaysLoad": true and returns the copy's
// path. Claude Code otherwise defers MCP tools behind its tool search,
// so the model sees only the tool's name and must spend a step loading
// it before every call. Other servers keep the runtime's default.
func alwaysLoadToolServer(mcpConfigPath string) (string, error) {
	if filepath.Base(filepath.Dir(mcpConfigPath)) != workspacekit.SortieDir {
		return "", fmt.Errorf("%s is not inside a %s directory", mcpConfigPath, workspacekit.SortieDir)
	}
	workspacePath := filepath.Dir(filepath.Dir(mcpConfigPath))
	f, err := workspacekit.OpenSortieFile(workspacePath, filepath.Base(mcpConfigPath))
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable after data is read

	raw, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(doc["mcpServers"], &servers); err != nil {
		return "", fmt.Errorf("mcpServers: %w", err)
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(servers[toolServerName], &entry); err != nil || entry == nil {
		return "", fmt.Errorf("mcpServers.%s is not an object", toolServerName)
	}

	entry["alwaysLoad"] = json.RawMessage("true")
	if servers[toolServerName], err = json.Marshal(entry); err != nil {
		return "", err
	}
	if doc["mcpServers"], err = json.Marshal(servers); err != nil {
		return "", err
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}

	if err := workspacekit.WriteSortieFile(workspacePath, alwaysLoadConfigName, encoded); err != nil {
		return "", err
	}
	return filepath.Join(workspacePath, workspacekit.SortieDir, alwaysLoadConfigName), nil
}
