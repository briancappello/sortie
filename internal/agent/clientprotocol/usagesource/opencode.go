package usagesource

import (
	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/registry"
)

const opencodeAgentName = "opencode"

const opencodeExecutableName = "opencode"

const opencodePackageName = "opencode-ai"

// opencodeDenyAllConfig is the inline configuration that removes the tool
// definitions from the request, tool-server tools included, and ends the tool
// calls a verification prompt would otherwise provoke. The runtime appends a
// rule after the configuration that keeps the skill tool declared, so no
// configuration removes that one.
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
		registry.VerificationKeepsSomeTools,
		registry.VerificationKeepsProjectInstructions,
		registry.VerificationKeepsUserInstructions,
		registry.VerificationKeepsSystemPrompt,
	)
}

func (opencodeVerification) Close() {}
