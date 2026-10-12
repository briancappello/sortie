//go:build unix

package clientprotocol

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestHostPrefixConformance(t *testing.T) {
	adapter, err := NewClientProtocolAdapter()
	if err != nil {
		t.Fatalf("NewClientProtocolAdapter() error = %v", err)
	}

	credentialtest.AssertHostPrefix(t, "agent-client-protocol", adapter, domain.AgentConfig{ReadTimeoutMS: 5000})
}
