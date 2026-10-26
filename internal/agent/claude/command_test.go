package claude

import (
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// assertHasArgPair fails if flag and value do not appear as consecutive
// elements in args.
func assertHasArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("buildArgs() missing %q %q in [%s]", flag, value, strings.Join(args, " "))
}

// assertNoFlag fails if flag appears anywhere in args.
func assertNoFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if slices.Contains(args, flag) {
		t.Errorf("buildArgs() unexpectedly contains flag %q in [%s]", flag, strings.Join(args, " "))
		return
	}
}

// newFirstTurnState returns a sessionState suitable for a first-turn invocation.
func newFirstTurnState(mcpConfigPath string) *sessionState {
	return &sessionState{
		claudeSessionID: "test-session-id",
		isContinuation:  false,
		mcpConfigPath:   mcpConfigPath,
	}
}

// TestParsePassthroughConfig_TypeFault covers the funnel's fault path for
// a wrong-typed string field: it returns the zero passthroughConfig and a
// fault whose rendering names the key and the type found. Every string
// key the funnel reads gets its own case, so a key whose fault arm was
// never wired shows up as a gap here rather than at runtime.
func TestParsePassthroughConfig_TypeFault(t *testing.T) {
	t.Parallel()

	keys := []string{
		"permission_mode",
		"model",
		"fallback_model",
		"effort",
		"allowed_tools",
		"disallowed_tools",
		"system_prompt",
		"mcp_config",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			pt, fault := parsePassthroughConfig(map[string]any{key: 123})

			if fault == nil {
				t.Fatalf("parsePassthroughConfig(%s=123) fault = nil, want non-nil", key)
			}
			if fault.Key != key {
				t.Errorf("parsePassthroughConfig(%s=123) fault.Key = %q, want %q", key, fault.Key, key)
			}
			wantErr := key + ": expected string, got integer"
			if fault.Error() != wantErr {
				t.Errorf("parsePassthroughConfig(%s=123) fault.Error() = %q, want %q", key, fault.Error(), wantErr)
			}
			if pt != (passthroughConfig{}) {
				t.Errorf("parsePassthroughConfig(%s=123) passthroughConfig = %+v, want zero value", key, pt)
			}
		})
	}
}

// TestMCPInjectionConformance proves claude-code's real buildArgs
// output carries the generated MCP config path, matching its declared
// disposition.
func TestMCPInjectionConformance(t *testing.T) {
	t.Parallel()

	declared, ok := registry.Agents.Meta("claude-code")
	if !ok {
		t.Fatal(`registry.Agents.Meta("claude-code") reported not registered`)
	}

	const mcpConfigPath = "/ws/.sortie/mcp.json"
	state := newFirstTurnState(mcpConfigPath)
	args := buildArgs(state, 1, "do work", passthroughConfig{SessionPersistence: true})

	agenttest.AssertMCPInjection(t, declared.MCPInjection, mcpConfigPath, agenttest.MCPLaunchSurface{Args: args})
}

func TestBuildArgs_MCPConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		mcpConfigPath string
		ptMCPConfig   string
		checkArgs     func(t *testing.T, args []string)
	}{
		{
			// Worker-generated config takes priority over operator config.
			name:          "worker path takes priority over operator",
			mcpConfigPath: "/ws/.sortie/mcp.json",
			ptMCPConfig:   "/op/mcp.json",
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--mcp-config", "/ws/.sortie/mcp.json")
			},
		},
		{
			// Operator config is used when no worker config is present.
			name:          "operator config used when worker path absent",
			mcpConfigPath: "",
			ptMCPConfig:   "/op/mcp.json",
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--mcp-config", "/op/mcp.json")
			},
		},
		{
			// Neither set: no --mcp-config flag emitted.
			name:          "neither set produces no mcp config flag",
			mcpConfigPath: "",
			ptMCPConfig:   "",
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				assertNoFlag(t, args, "--mcp-config")
			},
		},
		{
			// Only worker path set, no operator config.
			name:          "worker path used when operator config absent",
			mcpConfigPath: "/ws/.sortie/mcp.json",
			ptMCPConfig:   "",
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--mcp-config", "/ws/.sortie/mcp.json")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newFirstTurnState(tt.mcpConfigPath)
			pt := passthroughConfig{MCPConfig: tt.ptMCPConfig, SessionPersistence: true}
			args := buildArgs(state, 1, "do work", pt)
			tt.checkArgs(t, args)
		})
	}
}

func TestBuildArgs_CredentialVerificationSwitches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                            string
		verification, full, wantReduced bool
	}{
		{name: "working launch"},
		{name: "verification launch", verification: true, wantReduced: true},
		{name: "verification launch with full context", verification: true, full: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			command := agenttest.FakeRuntime(t, t.TempDir(), "fake-claude", agenttest.OutputScenario, agenttest.Output{})
			adapter, _ := NewClaudeCodeAdapter()
			session, err := adapter.StartSession(t.Context(), domain.StartSessionParams{
				WorkspacePath:           t.TempDir(),
				AgentConfig:             domain.AgentConfig{Command: command},
				CredentialVerification:  tt.verification,
				VerificationFullContext: tt.full,
				Settings:                map[string]any{"system_prompt": "operator-sentinel", "mcp_config": "/op/mcp.json"},
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			state := session.Internal.(*sessionState)

			args := buildArgs(state, 1, "Say exactly: SORTIE_CREDENTIAL_OK", state.passthrough)

			if tt.verification {
				assertHasArgPair(t, args, "--tools", "")
				assertHasFlag(t, args, "--strict-mcp-config")
				assertHasFlag(t, args, "--no-session-persistence")
				assertNoFlag(t, args, "--mcp-config")
			}
			if !tt.wantReduced {
				assertNoFlag(t, args, "--safe-mode")
				assertNoFlag(t, args, "--system-prompt")
				assertHasArgPair(t, args, "--append-system-prompt", "operator-sentinel")
				return
			}
			assertNoFlag(t, args, "--append-system-prompt")
			assertHasArgPair(t, args, "--system-prompt", agentcore.VerificationSystemPrompt)
			if n, m := countArg(args, "--safe-mode"), countArg(args, "--system-prompt"); n != 1 || m != 1 {
				t.Errorf("buildArgs() carries --safe-mode %d and --system-prompt %d times, want 1 and 1 in [%s]", n, m, strings.Join(args, " "))
			}
		})
	}
}

func countArg(args []string, arg string) (n int) {
	for _, a := range args {
		if a == arg {
			n++
		}
	}
	return n
}

func assertHasFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if !slices.Contains(args, flag) {
		t.Errorf("buildArgs() missing flag %q in [%s]", flag, strings.Join(args, " "))
	}
}
