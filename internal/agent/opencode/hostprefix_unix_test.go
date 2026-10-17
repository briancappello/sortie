//go:build unix

package opencode

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestHostPrefixConformance(t *testing.T) {
	adapter, err := NewOpenCodeAdapter()
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter() error = %v", err)
	}

	credentialtest.AssertHostPrefix(t, "opencode", adapter, domain.AgentConfig{})
}
