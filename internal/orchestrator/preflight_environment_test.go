package orchestrator

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
	"github.com/sortie-ai/sortie/internal/registry"
)

// forwardingCarrier stands in for an environment that runs the agent
// elsewhere and forwards the host variables its block binds as
// credentials or lists in pass_env.
type forwardingCarrier struct {
	*environmenttest.Recorder
	cfg config.SmolvmConfig
}

var (
	_ environment.Carrier      = forwardingCarrier{}
	_ environment.EnvForwarder = forwardingCarrier{}
)

func (forwardingCarrier) LaunchArgv(_ environment.Workspace, launch domain.CarrierLaunch) ([]string, error) {
	return launch.Argv, nil
}

func (forwardingCarrier) ConnectionFailed(environment.Workspace, int, []string) bool { return false }

func (f forwardingCarrier) ForwardsEnv(name string) bool {
	for _, cred := range f.cfg.Credentials {
		if cred.Env == name {
			return true
		}
	}
	return slices.Contains(f.cfg.PassEnv, name)
}

// nonForwardingCarrier runs elsewhere and forwards no host variable.
type nonForwardingCarrier struct{ *environmenttest.Recorder }

func (nonForwardingCarrier) LaunchArgv(_ environment.Workspace, launch domain.CarrierLaunch) ([]string, error) {
	return launch.Argv, nil
}

func (nonForwardingCarrier) ConnectionFailed(environment.Workspace, int, []string) bool { return false }

// credentialPreflight returns preflight parameters whose configuration is
// cfg, whose agent kinds declare the credential variables in kinds, and
// whose host environment holds the variables in hostEnv.
func credentialPreflight(cfg config.ServiceConfig, kinds map[string][]string, hostEnv map[string]string) PreflightParams {
	params := validPreflightParams()
	params.ConfigFunc = func() config.ServiceConfig { return cfg }
	params.AgentRegistry = &stubAgentRegistry{
		getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
		metaFunc: func(kind string) (registry.AgentMeta, bool) {
			names, ok := kinds[kind]
			if !ok {
				return registry.AgentMeta{}, false
			}
			return registry.AgentMeta{MCPDialect: registry.MCPDialectMCPServersJSON, CredentialEnv: registry.DeclareCredentialEnv(names...)}, true
		},
	}
	params.EnvironmentFunc = func(envCfg config.EnvironmentConfig) (environment.Environment, error) {
		return forwardingCarrier{Recorder: &environmenttest.Recorder{KindName: "vm"}, cfg: envCfg.Smolvm}, nil
	}
	params.LookupEnv = func(name string) (string, bool) {
		v, ok := hostEnv[name]
		return v, ok
	}
	return params
}

func credentialPreflightConfig(smolvm config.SmolvmConfig) config.ServiceConfig {
	return config.ServiceConfig{
		Tracker:     config.TrackerConfig{Kind: "test-tracker", APIKey: "secret", ActiveStates: []string{"To Do"}},
		Agent:       config.AgentConfig{Kind: "claude-code", Command: "/usr/bin/agent"},
		Environment: config.EnvironmentConfig{Kind: "vm", Smolvm: smolvm},
	}
}

func credentialErrors(result PreflightResult) []string {
	var msgs []string
	for _, e := range result.Errors {
		if e.Check == "environment.agent_credential" {
			msgs = append(msgs, e.Message)
		}
	}
	return msgs
}

func TestValidateDispatchConfig_AgentCredentialReachesTheEnvironment(t *testing.T) {
	t.Parallel()

	claude := map[string][]string{"claude-code": {"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"}}
	tests := []struct {
		name    string
		smolvm  config.SmolvmConfig
		hostEnv map[string]string
		want    []string // variables the errors name, in order
	}{
		{
			name:    "bound as a credential",
			smolvm:  config.SmolvmConfig{Credentials: []config.SmolvmCredential{{Name: "anthropic", Env: "ANTHROPIC_API_KEY", Hosts: []string{"api.anthropic.com"}}}},
			hostEnv: map[string]string{"ANTHROPIC_API_KEY": "sk-host"},
		},
		{
			name:    "listed in pass_env",
			smolvm:  config.SmolvmConfig{PassEnv: []string{"ANTHROPIC_API_KEY"}},
			hostEnv: map[string]string{"ANTHROPIC_API_KEY": "sk-host"},
		},
		{
			name:    "unset on the host",
			hostEnv: map[string]string{"OPENROUTER_API_KEY": "unrelated"},
		},
		{
			name:    "set and neither bound nor passed",
			smolvm:  config.SmolvmConfig{PassEnv: []string{"OTHER"}},
			hostEnv: map[string]string{"ANTHROPIC_API_KEY": "sk-host"},
			want:    []string{"ANTHROPIC_API_KEY"},
		},
		{
			name:    "set to the empty string",
			hostEnv: map[string]string{"ANTHROPIC_AUTH_TOKEN": ""},
			want:    []string{"ANTHROPIC_AUTH_TOKEN"},
		},
		{
			name:    "every unforwarded variable is named",
			smolvm:  config.SmolvmConfig{PassEnv: []string{"ANTHROPIC_API_KEY"}},
			hostEnv: map[string]string{"ANTHROPIC_API_KEY": "sk-host", "ANTHROPIC_AUTH_TOKEN": "token"},
			want:    []string{"ANTHROPIC_AUTH_TOKEN"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ValidateDispatchConfig(credentialPreflight(credentialPreflightConfig(tt.smolvm), claude, tt.hostEnv))
			msgs := credentialErrors(result)
			if len(msgs) != len(tt.want) {
				t.Fatalf("credential errors = %q, want %d naming %q", msgs, len(tt.want), tt.want)
			}
			for i, name := range tt.want {
				if !strings.Contains(msgs[i], " "+name+",") || !strings.Contains(msgs[i], `"claude-code"`) || !strings.Contains(msgs[i], `"vm"`) {
					t.Errorf("error %d = %q, want it to name %s, the agent kind, and the environment", i, msgs[i], name)
				}
				if strings.Contains(msgs[i], tt.hostEnv[name]) && tt.hostEnv[name] != "" {
					t.Errorf("error %d = %q carries the variable's value", i, msgs[i])
				}
			}
			if len(tt.want) > 0 && (result.OK() || !strings.Contains(result.Error(), tt.want[0])) {
				t.Errorf("result OK = %v, Error() = %q, want dispatch refused naming %s", result.OK(), result.Error(), tt.want[0])
			}
		})
	}
}

func TestValidateDispatchConfig_AgentCredentialEveryReachedKind(t *testing.T) {
	t.Parallel()

	cfg := credentialPreflightConfig(config.SmolvmConfig{})
	cfg.Dispatch = config.DispatchConfig{Rules: []config.DispatchRule{{
		Name: "copilot-rule", Match: config.DispatchMatch{Labels: []string{"x"}}, Selection: config.DispatchSelection{AgentKind: "copilot-cli"},
	}}}
	kinds := map[string][]string{
		"claude-code": {"ANTHROPIC_API_KEY"},
		"copilot-cli": {"GH_TOKEN"},
		"unreached":   {"UNREACHED_TOKEN"},
	}
	hostEnv := map[string]string{"GH_TOKEN": "gh", "UNREACHED_TOKEN": "u"}

	msgs := credentialErrors(ValidateDispatchConfig(credentialPreflight(cfg, kinds, hostEnv)))
	if len(msgs) != 1 || !strings.Contains(msgs[0], "GH_TOKEN") || !strings.Contains(msgs[0], `"copilot-cli"`) {
		t.Errorf("credential errors = %q, want one naming GH_TOKEN of the kind a dispatch rule routes to, and none for a kind no selector reaches", msgs)
	}
}

func TestValidateDispatchConfig_AgentCredentialOnlyForEnvironmentsElsewhere(t *testing.T) {
	t.Parallel()

	kinds := map[string][]string{"claude-code": {"ANTHROPIC_API_KEY"}}
	hostEnv := map[string]string{"ANTHROPIC_API_KEY": "sk-host"}
	tests := []struct {
		name string
		env  EnvironmentFunc
		want int
	}{
		{name: "environment on this host", env: func(config.EnvironmentConfig) (environment.Environment, error) {
			return &environmenttest.Recorder{KindName: config.EnvironmentKindLocal}, nil
		}},
		{name: "environment that cannot be built", env: func(config.EnvironmentConfig) (environment.Environment, error) {
			return nil, errors.New("no such kind")
		}},
		{name: "carrier that forwards nothing", env: func(config.EnvironmentConfig) (environment.Environment, error) {
			return nonForwardingCarrier{&environmenttest.Recorder{KindName: "vm"}}, nil
		}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			params := credentialPreflight(credentialPreflightConfig(config.SmolvmConfig{}), kinds, hostEnv)
			params.EnvironmentFunc = tt.env
			if got := credentialErrors(ValidateDispatchConfig(params)); len(got) != tt.want {
				t.Errorf("credential errors = %q, want %d", got, tt.want)
			}
		})
	}
}

func TestValidateDispatchConfig_AgentCredentialDefaultEnvironmentIsLocal(t *testing.T) {
	t.Parallel()

	// Without an EnvironmentFunc the block's kind is built through the
	// registry: the default local kind runs on this host.
	params := credentialPreflight(credentialPreflightConfig(config.SmolvmConfig{}), map[string][]string{"claude-code": {"ANTHROPIC_API_KEY"}},
		map[string]string{"ANTHROPIC_API_KEY": "sk-host"})
	params.EnvironmentFunc = nil
	cfg := params.ConfigFunc()
	cfg.Environment = config.EnvironmentConfig{Kind: config.EnvironmentKindLocal}
	params.ConfigFunc = func() config.ServiceConfig { return cfg }
	if got := credentialErrors(ValidateDispatchConfig(params)); len(got) != 0 {
		t.Errorf("credential errors = %q, want none under the local kind", got)
	}
}
