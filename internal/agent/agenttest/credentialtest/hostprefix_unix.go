//go:build unix

package credentialtest

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// hostPrefixArg is the argument the conformance host prefix carries after
// its program: the separator the stand-in prefix takes as its own.
const hostPrefixArg = "--"

// hostPrefixToken is the session token the conformance sessions set.
const hostPrefixToken = "sortie-host-prefix-conformance-token" //nolint:gosec // a fixed test value, not a credential

// AssertHostPrefix fails t unless kind's sessions launch behind a host
// prefix exactly where it belongs. adapter and config are as for
// [AssertEarlyExitReport], whose conformance runtime every case launches.
// For each command form, locally and through an execution environment
// carrier, a verification session through [agentcore.VerifyCredential]
// and a working session each run with HostPrefix set to a stand-in
// program and its one argument, and must fail with the shared early-exit
// report, which only the runtime behind the prefix produces. Every run
// of the prefix must carry its argument first, then the launch: the
// configured command locally, and the carrier's launch command through
// the carrier. Every run must see the session token in its environment,
// and none may be an auxiliary launch such as a version query. Locally,
// the prefix's runs must also match, in number, program, and leading
// arguments, those of the same sessions with the prefix written in front
// of the configured command instead, leaving out that setup's version
// queries, which run behind it only there.
func AssertHostPrefix(t *testing.T, kind string, adapter domain.AgentAdapter, config domain.AgentConfig) {
	t.Helper()

	binDir := t.TempDir()
	runtimePath := agenttest.FakeRuntime(t, binDir, "agent", agenttest.OutputScenario, agenttest.Output{
		Stderr:   earlyExitConformanceLine + "\n",
		ExitCode: 2,
		WhenArg:  earlyExitConformanceSwitch,
		Version:  conformanceRuntimeVersion,
	})

	for _, form := range commandForms {
		formConfig := form.apply(config, runtimePath)
		agentArgv := []string{runtimePath, earlyExitConformanceSwitch}

		for _, carried := range []bool{false, true} {
			mode := "local"
			if carried {
				mode = "carrier"
			}
			for _, verification := range []bool{true, false} {
				name := fmt.Sprintf("%s (%s, %s): %s", kind, form.name, mode, sessionName(verification))
				prefixPath, runs := agenttest.WriteHostPrefix(t, t.TempDir(), "host-prefix", "SORTIE_SESSION_TOKEN")
				var carrier *agenttest.FakeCarrier
				params := hostPrefixParams(t, formConfig, verification)
				params.HostPrefix = []string{prefixPath, hostPrefixArg}
				if carried {
					carrier = &agenttest.FakeCarrier{}
					params.Carrier = carrier
				}

				assertEarlyExitReportOutcome(t, name, runHostPrefixSession(adapter, params))

				got := runs()
				if len(got) == 0 {
					t.Errorf("case %q: the host prefix never ran, want every session launch behind it", name)
					continue
				}
				for i, run := range got {
					assertHostPrefixRun(t, fmt.Sprintf("%s: prefix run %d", name, i), run, prefixPath, agentArgv, carrier)
				}

				if carried {
					continue
				}
				todayPath, todayRuns := agenttest.WriteHostPrefix(t, t.TempDir(), "host-prefix", "SORTIE_SESSION_TOKEN")
				today := hostPrefixParams(t, prefixedConfig(form, config, runtimePath, todayPath), verification)
				assertEarlyExitReportOutcome(t, name+" with the prefix in the agent command", runHostPrefixSession(adapter, today))
				assertSameLaunches(t, name, got, todayRuns(), prefixPath, todayPath, 1+len(agentArgv))
			}
		}
	}
}

func sessionName(verification bool) string {
	if verification {
		return "verification session"
	}
	return "working session"
}

func hostPrefixParams(t *testing.T, config domain.AgentConfig, verification bool) domain.StartSessionParams {
	t.Helper()
	return domain.StartSessionParams{
		WorkspacePath:          t.TempDir(),
		AgentConfig:            config,
		CredentialVerification: verification,
		SessionEnv: []domain.EnvVar{
			{Name: "SORTIE_ISSUE_ID", Value: "10001"},
			{Name: "SORTIE_SESSION_TOKEN", Value: hostPrefixToken},
		},
	}
}

// prefixedConfig is form's command with the conformance prefix, whose
// program is prefixPath, written in front of it.
func prefixedConfig(form commandForm, config domain.AgentConfig, runtimePath, prefixPath string) domain.AgentConfig {
	config = form.apply(config, runtimePath)
	if len(config.CommandArgv) > 0 {
		config.CommandArgv = append([]string{prefixPath, hostPrefixArg}, config.CommandArgv...)
		return config
	}
	config.Command = prefixPath + " " + hostPrefixArg + " " + config.Command
	return config
}

func runHostPrefixSession(adapter domain.AgentAdapter, params domain.StartSessionParams) error {
	if params.CredentialVerification {
		_, err := agentcore.VerifyCredential(context.Background(), adapter, agentcore.CredentialVerification{Session: params})
		return err
	}
	return runWorking(context.Background(), adapter, params, 0, 0)
}

func assertHostPrefixRun(t *testing.T, name string, run agenttest.HostPrefixRun, prefixPath string, agentArgv []string, carrier *agenttest.FakeCarrier) {
	t.Helper()

	if run.Program != prefixPath {
		t.Errorf("case %q: program = %q, want %q", name, run.Program, prefixPath)
	}
	if run.Env["SORTIE_SESSION_TOKEN"] != hostPrefixToken {
		t.Errorf("case %q: SORTIE_SESSION_TOKEN = %q, want the session token on the host", name, run.Env["SORTIE_SESSION_TOKEN"])
	}
	if slices.Contains(run.Args, "--version") {
		t.Errorf("case %q: args %q hold a version query, want no auxiliary launch behind the prefix", name, run.Args)
	}
	if len(run.Args) == 0 || run.Args[0] != hostPrefixArg {
		t.Errorf("case %q: args = %q, want %q first", name, run.Args, hostPrefixArg)
		return
	}
	launch := run.Args[1:]
	if carrier == nil {
		if len(launch) < len(agentArgv) || !slices.Equal(launch[:len(agentArgv)], agentArgv) {
			t.Errorf("case %q: launch = %q, want the configured command %q right after the prefix", name, launch, agentArgv)
		}
		return
	}
	for _, carriedLaunch := range carrier.Launches() {
		argv, err := (&agenttest.FakeCarrier{}).LaunchArgv(carriedLaunch)
		if err == nil && slices.Equal(argv, launch) {
			return
		}
	}
	t.Errorf("case %q: launch = %q, want one of the carrier's launch commands right after the prefix", name, launch)
}

// assertSameLaunches fails t unless every run in got, in order, matches
// a run in want, in the same order: started from its own setup's prefix
// program, with as many arguments, and the same first n of them. The
// runs of want no run of got matches are the auxiliary launches, such as
// a session deletion, that run behind the prefix only when it is written
// into the agent command.
func assertSameLaunches(t *testing.T, name string, got, want []agenttest.HostPrefixRun, gotPrefix, wantPrefix string, n int) {
	t.Helper()

	next := 0
	for i, run := range got {
		matched := false
		for next < len(want) {
			candidate := want[next]
			next++
			if run.Program == gotPrefix && candidate.Program == wantPrefix &&
				len(run.Args) == len(candidate.Args) && len(run.Args) >= n && slices.Equal(run.Args[:n], candidate.Args[:n]) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("case %q: session launch %d %q %q matches no launch of the prefix written into the agent command, %q", name, i, run.Program, run.Args, want)
			return
		}
	}
}
