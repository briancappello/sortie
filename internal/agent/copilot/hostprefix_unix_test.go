//go:build unix

package copilot

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestHostPrefixConformance(t *testing.T) {
	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter() error = %v", err)
	}

	credentialtest.AssertHostPrefix(t, "copilot-cli", adapter, domain.AgentConfig{})
}
