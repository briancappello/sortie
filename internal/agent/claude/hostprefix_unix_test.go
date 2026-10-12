//go:build unix

package claude

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestHostPrefixConformance(t *testing.T) {
	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter() error = %v", err)
	}

	credentialtest.AssertHostPrefix(t, "claude-code", adapter, domain.AgentConfig{})
}
