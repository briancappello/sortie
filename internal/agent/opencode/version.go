package opencode

import (
	"context"
	"fmt"
	"os"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
)

// checkRuntimeVersion launches the configured command with --version,
// bounded by [agentcore.AuxiliaryTimeout], and refuses a command that
// reports no readable version or a major other than 2. It re-verifies the
// workspace through the same [agentcore.LaunchTarget.AuxiliaryCommand]
// path every other auxiliary launch uses, so a session start on a
// removed workspace fails here rather than at the first turn.
func checkRuntimeVersion(ctx context.Context, state *sessionState) *domain.AgentError {
	queryCtx, cancel := context.WithTimeout(ctx, agentcore.AuxiliaryTimeout(state.agentConfig))
	defer cancel()

	stdout := procutil.NewTailBuffer(agentcore.EarlyExitCaptureBytes)
	stderr := procutil.NewTailBuffer(agentcore.EarlyExitCaptureBytes)

	cmd, agentErr := state.target.AuxiliaryCommand(queryCtx, []string{"--version"}, nil, versionQueryEnv(os.Environ()),
		sshutil.EnvVar{Name: "OPENCODE_DISABLE_AUTOUPDATE", Value: "true"})
	if agentErr != nil {
		return agentErr
	}

	result, startErr := procutil.RunCapture(queryCtx, cmd, procutil.StopGrace(state.agentConfig.StopGraceMS), procutil.CaptureParams{
		Stdout: stdout,
		Stderr: stderr,
		Logger: state.logger(),
	})

	switch {
	case startErr != nil:
		if cancelled := agentcore.SessionStartCancelledError(ctx, startErr); cancelled != nil {
			return cancelled
		}
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "could not start the agent runtime to read its version",
			Err:     startErr,
		}
	case ctx.Err() != nil:
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "the session start ended while the agent runtime was reporting its version",
			Err:     ctx.Err(),
		}
	case queryCtx.Err() != nil:
		return &domain.AgentError{
			Kind: domain.ErrResponseTimeout,
			Message: fmt.Sprintf("the agent runtime did not report its version within %d ms",
				agentcore.AuxiliaryTimeout(state.agentConfig).Milliseconds()),
		}
	case result.WaitErr != nil:
		if state.target.RemoteCommand != "" && sshutil.ConnectionFailed(procutil.ExtractExitCode(result.WaitErr)) {
			return agentcore.ConnectionFailedError()
		}
		return agentcore.ExitedEarly(state.target, result).Report(stderr.Collector(state.logger()))
	}

	version, major, ok := agentcore.ParseRuntimeVersion(stdout.Bytes())
	if !ok || stdout.Truncated() {
		message := "no output"
		if line := agentcore.FirstReadableLine(stdout.Bytes()); line != "" {
			message = redact.Truncate(line, 200)
		}
		return &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "the configured OpenCode command reported no version Sortie can read: " + message,
		}
	}
	if major != 2 {
		return &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: fmt.Sprintf("OpenCode %s is not supported; install OpenCode 2.x, published on npm as @opencode/cli", version),
		}
	}

	return nil
}

// versionQueryEnv scrubs base of every managed variable a turn's own
// environment carries and appends the one setting the version query
// itself needs, so the query never carries a tool policy or a sharing
// setting.
func versionQueryEnv(base []string) []string {
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if shouldDropManagedEnv(entry) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "OPENCODE_DISABLE_AUTOUPDATE=true")
}
