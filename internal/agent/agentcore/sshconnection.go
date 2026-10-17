package agentcore

import (
	"errors"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// ErrEnvironmentConnectionFailed marks an execution environment that
// failed before the agent command started in it, such as one that is not
// running, as opposed to an agent command that failed there.
var ErrEnvironmentConnectionFailed = errors.New("the execution environment failed before the agent command started")

// ConnectionFailedError is the error for an ssh client that exited
// with the status OpenSSH reserves for its own connection failures.
func ConnectionFailedError() *domain.AgentError {
	return &domain.AgentError{
		Kind:    domain.ErrPortExit,
		Message: "ssh connection failed",
		Err:     sshutil.ErrConnectionFailed,
	}
}

// EnvironmentConnectionFailedError is the error for an execution
// environment's launch that failed before the agent command started. It
// is classified and retried as [ConnectionFailedError] is.
func EnvironmentConnectionFailedError() *domain.AgentError {
	return &domain.AgentError{
		Kind:    domain.ErrPortExit,
		Message: "execution environment connection failed",
		Err:     ErrEnvironmentConnectionFailed,
	}
}

// IsConnectionFailure reports whether err records a remote launch that
// failed to reach the agent: an SSH connection failure or an execution
// environment that failed before the agent command started.
func IsConnectionFailure(err error) bool {
	return errors.Is(err, sshutil.ErrConnectionFailed) || errors.Is(err, ErrEnvironmentConnectionFailed)
}

// ConnectionFailedForRequest reports whether an ssh connection failure
// should replace the request's own outcome: sshFailed holds and the
// request produced no terminal result.
func ConnectionFailedForRequest(sshFailed, hasTerminalResult bool) bool {
	return sshFailed && !hasTerminalResult
}

// ReaperConnectionFailed reports whether a remote session's ssh subprocess
// exited with OpenSSH's connection-failure status. It waits up to drainGrace
// so a caller that saw the stream end does not race the reap.
func ReaperConnectionFailed(remote bool, reaper *procutil.Reaper, drainGrace time.Duration) bool {
	if !remote {
		return false
	}
	return reapedConnectionFailed(reaper, drainGrace, sshutil.ConnectionFailed)
}

// TargetReaperConnectionFailed reports whether target's remote session
// subprocess failed to reach the agent, as [LaunchTarget.ConnectionFailed]
// classifies its exit status and stderrTail. It waits up to drainGrace so
// a caller that saw the stream end does not race the reap, and is false
// in local mode.
func TargetReaperConnectionFailed(target LaunchTarget, reaper *procutil.Reaper, drainGrace time.Duration, stderrTail func() []string) bool {
	if !target.Remote() {
		return false
	}
	return reapedConnectionFailed(reaper, drainGrace, func(exitCode int) bool {
		return target.ConnectionFailed(exitCode, stderrTail)
	})
}

func reapedConnectionFailed(reaper *procutil.Reaper, drainGrace time.Duration, failed func(exitCode int) bool) bool {
	if reaper == nil {
		return false
	}
	if drainGrace <= 0 {
		drainGrace = procutil.DefaultDrainGrace
	}
	select {
	case <-reaper.Done():
	case <-time.After(drainGrace):
		return false
	}
	return failed(procutil.ExtractExitCode(reaper.Err()))
}
