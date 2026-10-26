package usagesource

import (
	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/registry"
)

const opencodeAgentName = "opencode"

const opencodeExecutableName = "opencode"

const opencodePackageName = "opencode-ai"

// opencodeDenyAllConfig is the inline configuration that removes every tool
// definition from the request, tool-server and skill tools included, and ends
// the tool calls a verification prompt would otherwise provoke.
const opencodeDenyAllConfig = `{"permission":{"*":"deny"}}`

func init() {
	VerificationAdditions.Register(opencodeAgentName, func() VerificationAddition { return opencodeVerification{} })
}

type opencodeVerification struct{}

func (opencodeVerification) Claim(target agentcore.LaunchTarget) (VerificationSwitches, bool) {
	if !namesRuntime(target, opencodeExecutableName, opencodePackageName) {
		return VerificationSwitches{}, false
	}
	return VerificationSwitches{Env: []string{"OPENCODE_CONFIG_CONTENT=" + opencodeDenyAllConfig}}, true
}

func (opencodeVerification) Context() registry.VerificationContext {
	return registry.DeclareVerificationContext(
		registry.VerificationKeepsProjectInstructions,
		registry.VerificationKeepsUserInstructions,
		registry.VerificationKeepsSystemPrompt,
	)
}

func (opencodeVerification) Close() {}
