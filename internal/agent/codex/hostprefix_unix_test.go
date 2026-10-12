//go:build unix

package codex

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestHostPrefixConformance(t *testing.T) {
	adapter, err := NewCodexAdapter()
	if err != nil {
		t.Fatalf("NewCodexAdapter() error = %v", err)
	}

	credentialtest.AssertHostPrefix(t, "codex", adapter, domain.AgentConfig{})
}
