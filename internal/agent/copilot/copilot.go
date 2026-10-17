// Package copilot implements [domain.AgentAdapter] for the GitHub
// Copilot CLI. It launches the copilot binary as a subprocess in
// headless mode, reads newline-delimited JSON from stdout, and
// normalizes events into domain types. Registered under kind
// "copilot-cli" via an init function. Safe for concurrent use across
// sessions: callers may invoke [CopilotAdapter.RunTurn] concurrently
// for different [domain.Session] instances, but turns for a single
// session must be serialized.
package copilot

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
)

// sessionIDPattern matches the safe path-segment character set a
// copilot session id must satisfy before it is joined into a
// session-state path.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// copilotToolDeniedCode is the tool.execution_complete error code the CLI
// reports when its own non-interactive permission policy refuses a tool
// call and the session continues.
const copilotToolDeniedCode = "denied"

// defaultCommand is what a session launches when it is given no command.
const defaultCommand = "copilot"

func init() {
	registry.Agents.RegisterWithMeta("copilot-cli", NewCopilotAdapter, registry.AgentMeta{
		RequiresCommand:     true,
		DefaultCommand:      defaultCommand,
		ValidateAgentConfig: validateConfig,
		MCPInjection:        registry.MCPInjectionSupported,
		UsageArrival:        registry.UsageArrivalTurnEnd,
		UsageAttribution:    registry.UsageAttributionPerModel,
		CredentialEnv:       registry.DeclareCredentialEnv("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"),
		EffortForwarding:    registry.EffortForwarded,
		UsageSessionRules: []registry.UsageSessionRule{
			{
				// Mirrors sessionState.recoverUsage's own remote
				// condition: over SSH, the post-exit journal read is
				// skipped entirely, so nothing ever arrives.
				When:        func(passthrough map[string]any, remote bool) bool { return remote },
				Arrival:     registry.UsageArrivalNone,
				Attribution: registry.UsageAttributionNone,
			},
		},
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CopilotAdapter)(nil)

// CopilotAdapter satisfies [domain.AgentAdapter] by managing Copilot
// CLI subprocesses. One adapter instance serves all concurrent
// sessions; per-session state is held in [sessionState] via the
// [domain.Session] Internal field.
type CopilotAdapter struct{}

// sessionState is adapter-internal state stored in [domain.Session]
// Internal. It tracks the Copilot CLI session ID and per-turn scan
// state across turns.
type sessionState struct {
	target           agentcore.LaunchTarget
	copilotSessionID string
	passthrough      passthroughConfig
	agentConfig      domain.AgentConfig
	baseLogger       *slog.Logger

	// mcpConfigPath is the worker-generated MCP config file path.
	mcpConfigPath string

	isContinuation bool

	credentialVerification bool

	// forkSession owns the subprocess lifecycle for this session.
	forkSession *agentcore.ForkPerTurnSession

	// usage holds the session's usage report. Constructed once in
	// StartSession and never reset between turns.
	usage *agentcore.TurnEndUsage

	// resume separates this run's spend from the usage the session held
	// when the run started. Never reset between turns.
	resume agentcore.ResumeBaseline

	// sessionIDRejectionLogged suppresses repeating the invalid-session-id
	// Warn on every turn of a run whose session id never becomes valid.
	sessionIDRejectionLogged bool

	// Per-turn scan state owned by the ParseLine and OnFinalize hook
	// closures. Reset at the top of each RunTurn call before delegating
	// to forkSession.
	inFlight *agentcore.ToolTracker
	work     *agentcore.WorkObserver

	// turnCompletionSeen is true once a session.task_complete event has
	// arrived this turn.
	turnCompletionSeen bool

	// turnCompletionSuccess is false only when a session.task_complete
	// payload parsed this turn and its success field was explicitly
	// false.
	turnCompletionSuccess bool

	// turnCompletionSummary is the session.task_complete report's
	// summary, empty when the report never arrived or its payload was
	// unreadable.
	turnCompletionSummary string

	// turnErrorMessage is the trimmed message of the turn's last
	// session.error event, empty when none arrived or its payload was
	// unreadable. It is the failure reason when the result event reports a
	// failed exit.
	turnErrorMessage string

	// turnContinuations counts the user.message events this turn whose
	// payload marked them as autopilot continuations. Diagnostic only;
	// never consulted by the turn disposition.
	turnContinuations int
}

func (s *sessionState) logger() *slog.Logger {
	if s.copilotSessionID == "" {
		return s.baseLogger
	}
	return logging.WithSession(s.baseLogger, s.copilotSessionID)
}

func (s *sessionState) refreshForkLogger() {
	if s.forkSession != nil {
		s.forkSession.SetLogger(s.logger())
	}
}

// recoverUsage reads the runtime's session-state journal after the
// subprocess has exited and returns this run's usage with the model the
// journal's newest record names, whether that figure is an estimate, and the
// session's own usage for hand-over to a later run.
//
// A skipped or failed read returns no figure, false, and no hand-over
// figure. A journal without a record, or one whose newest record equals the
// last reading, means the turn wrote none: no figure, and the session's
// usage is unchanged. A reading the baseline cannot place exactly returns no
// figure and true.
func (s *sessionState) recoverUsage(logger *slog.Logger) (recovered *agentcore.RecoveredUsage, estimated bool, resume *domain.TokenUsage) {
	remote := s.target.RemoteCommand != ""
	sessionID := s.copilotSessionID
	validID := sessionID != "" && sessionIDPattern.MatchString(sessionID)

	if remote || !validID {
		if sessionID != "" && !validID && !s.sessionIDRejectionLogged {
			logger.Warn("copilot session id rejected for session-state read",
				slog.String("reason", "invalid_session_id"))
			s.sessionIDRejectionLogged = true
		}
		return nil, false, nil
	}

	root, err := sessionStateRoot(os.Getenv, os.UserHomeDir)
	if err != nil {
		logger.Debug("copilot session-state root resolution failed",
			slog.String("session_id", sessionID), slog.String("reason", "root_unresolved"))
		return nil, false, nil
	}
	eventsPath := filepath.Join(root, sessionID, "events.jsonl")

	current, model, found, err := readSessionUsage(eventsPath)
	if err != nil {
		if errors.Is(err, errSessionStateCapExceeded) {
			logger.Warn("copilot session-state read abandoned",
				slog.String("session_id", sessionID), slog.String("reason", "cap_exceeded"))
		} else {
			logger.Debug("copilot session-state read failed",
				slog.String("session_id", sessionID), slog.String("reason", "unreadable"))
		}
		return nil, false, nil
	}

	if !found {
		logger.Debug("copilot session-state has no session.shutdown record yet",
			slog.String("session_id", sessionID), slog.String("reason", "no_record"))
		return nil, false, s.resume.Resume()
	}

	if last := s.resume.Resume(); last != nil && *last == current {
		return nil, false, last
	}

	run, estimated := s.resume.Observe(current, nil)
	if estimated {
		return nil, true, s.resume.Resume()
	}
	return &agentcore.RecoveredUsage{Run: run, Model: model}, false, s.resume.Resume()
}

// seedResumeBaseline returns the baseline a session starts from: zero for a
// session the run creates, the figure handed over for a resumed one, and
// otherwise the newest record of the resumed session's local journal. A
// remote launch, an invalid session id, or an unreadable journal leaves the
// baseline unknown; none fails the session start.
func seedResumeBaseline(params domain.StartSessionParams, target agentcore.LaunchTarget, sessionID string, logger *slog.Logger) agentcore.ResumeBaseline {
	switch {
	case params.ResumeSessionID == "":
		return *agentcore.NewResumeBaseline(&domain.TokenUsage{})
	case params.ResumeUsage != nil:
		return *agentcore.NewResumeBaseline(params.ResumeUsage)
	case target.RemoteCommand != "" || !sessionIDPattern.MatchString(sessionID):
		return agentcore.ResumeBaseline{}
	}
	return *agentcore.NewResumeBaseline(journalResumeUsage(sessionID, logger))
}

// journalResumeUsage returns the totals of the newest session.shutdown record
// in the session's journal, zero when the journal is missing or holds no
// record, and nil when the journal cannot be read.
func journalResumeUsage(sessionID string, logger *slog.Logger) *domain.TokenUsage {
	root, err := sessionStateRoot(os.Getenv, os.UserHomeDir)
	if err != nil {
		logger.Debug("copilot session-state root resolution failed", slog.String("reason", "root_unresolved"))
		return nil
	}

	current, _, found, err := readSessionUsage(filepath.Join(root, sessionID, "events.jsonl"))
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !found) {
		return &domain.TokenUsage{}
	}
	if err != nil {
		logger.Debug("copilot session-state baseline read failed", slog.String("reason", "unreadable"))
		return nil
	}
	return &current
}

// NewCopilotAdapter creates a [CopilotAdapter]. Settings and command
// resolution are deferred to [CopilotAdapter.StartSession].
func NewCopilotAdapter() (domain.AgentAdapter, error) {
	return &CopilotAdapter{}, nil
}

// StartSession parses the session's settings, validates the workspace path,
// resolves the copilot binary, and initializes per-session state. A local
// session runs a version canary of the binary; turns spawn their
// subprocess in [CopilotAdapter.RunTurn].
func (a *CopilotAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	pt, fault := parsePassthroughConfig(params.Settings)
	if fault != nil {
		return domain.Session{}, agentcore.SettingsError(fault.Error(), fault)
	}
	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	// The canary check is local-mode only.
	if target.RemoteCommand == "" {
		stopGrace := procutil.StopGrace(params.AgentConfig.StopGraceMS)

		canaryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(canaryCtx, target.Command, "--version") //nolint:gosec // target.Command from LookPath
		var combined bytes.Buffer
		result, startErr := procutil.RunCapture(canaryCtx, cmd, stopGrace, procutil.CaptureParams{Stdout: &combined, Stderr: &combined})
		if startErr != nil || result.WaitErr != nil {
			if cancelled := agentcore.SessionStartCancelledError(ctx, startErr); cancelled != nil {
				return domain.Session{}, cancelled
			}
			canaryErr := startErr
			if canaryErr == nil {
				canaryErr = result.WaitErr
			}
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "copilot binary found but not functional; ensure Node.js 22+ is available",
				Err:     canaryErr,
			}
		}
		slog.Debug("copilot version check passed", slog.String("version", strings.TrimSpace(combined.String())))
	}

	copilotSessionID := params.ResumeSessionID
	if copilotSessionID == "" {
		uuid, uuidErr := agentcore.NewUUIDv4()
		if uuidErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "could not generate a session id",
				Err:     uuidErr,
			}
		}
		copilotSessionID = uuid
	}

	baseLogger := slog.Default().With(slog.String("component", "copilot-adapter"))

	state := &sessionState{
		target:                 target,
		copilotSessionID:       copilotSessionID,
		passthrough:            pt,
		agentConfig:            params.AgentConfig,
		baseLogger:             baseLogger,
		mcpConfigPath:          params.MCPConfigPath,
		isContinuation:         params.ResumeSessionID != "",
		credentialVerification: params.CredentialVerification,
		usage:                  agentcore.NewTurnEndUsage(),
		resume:                 seedResumeBaseline(params, target, copilotSessionID, logging.WithSession(baseLogger, copilotSessionID)),
	}

	hooks := agentcore.ForkPerTurnHooks{
		BuildArgs: func(turn int, prompt string) []string {
			return buildArgs(state, turn, prompt, state.passthrough)
		},
		ParseLine: func(line []byte, emit func(domain.AgentEvent), pid string, spend *agentcore.TurnSpend) (any, error) {
			now := time.Now().UTC()

			event, parseErr := parseEvent(line)
			if parseErr != nil {
				return nil, parseErr
			}

			switch event.Type {
			case "assistant.message_delta":
				spend.MarkModelReached()
				// The event type itself names an assistant message, so it
				// is a work signal even though its payload is unparsed.
				state.work.ObserveAssistantOutput()
				// Stall timer reset; ephemeral streaming content.
				agentcore.EmitNotification(emit, "")

			case "assistant.message":
				spend.MarkModelReached()
				if len(event.Data) > 0 {
					msgData, dataErr := parseAssistantMessageData(event.Data)
					if dataErr == nil {
						if msgData.Content != "" {
							state.work.ObserveAssistantOutput()
						}
						if len(msgData.ToolRequests) > 0 {
							state.work.ObserveToolActivity()
						}
						agentcore.EmitNotification(emit, summarizeAssistantMessage(msgData))
					} else {
						state.logger().Debug("failed to parse assistant.message data", slog.Any("error", dataErr))
						agentcore.EmitNotification(emit, "")
					}
				}

			case "model.messages_snapshot":
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

			case "assistant.turn_start", "assistant.turn_end":
				agentcore.EmitNotification(emit, "")

			case "tool.execution_start":
				spend.MarkModelReached()
				if len(event.Data) > 0 {
					toolData, dataErr := parseToolExecutionData(event.Data)
					if dataErr == nil {
						state.work.ObserveToolActivity()
						state.inFlight.Begin(toolData.ToolCallID, toolData.ToolName)
						agentcore.EmitNotification(emit, "")
					}
				}

			case "tool.execution_complete":
				if len(event.Data) > 0 {
					toolData, dataErr := parseToolExecutionData(event.Data)
					if dataErr == nil {
						state.work.ObserveToolActivity()
						toolName, durationMS, ok := state.inFlight.End(toolData.ToolCallID)
						if !ok {
							toolName = toolData.ToolName
						}
						emit(domain.AgentEvent{
							Type:           domain.EventToolResult,
							Timestamp:      now,
							ToolName:       toolName,
							ToolDurationMS: durationMS,
							ToolError:      !toolData.Success,
						})

						// The CLI's own non-interactive permission policy
						// denied this tool call and the session continues;
						// --no-ask-user closes the human-question path, so
						// every recognized request here is a permission
						// request with no reply channel.
						if !toolData.Success && toolData.Error != nil && toolData.Error.Code == copilotToolDeniedCode {
							posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused)
							agentcore.EmitNotification(emit, posture.Notice)
						}
					}
				}

			case "session.warning":
				msg := ""
				if len(event.Data) > 0 {
					warnData, dataErr := parseSessionWarningData(event.Data)
					if dataErr == nil {
						msg = warnData.Message
					}
				}
				state.logger().Warn("copilot session warning", slog.String("message", cmp.Or(msg, "session warning")))
				agentcore.EmitNotification(emit, msg)

			case "session.info":
				msg := ""
				if len(event.Data) > 0 {
					infoData, dataErr := parseSessionInfoData(event.Data)
					if dataErr == nil {
						msg = infoData.Message
					}
				}
				agentcore.EmitNotification(emit, msg)

			case "session.task_complete":
				state.turnCompletionSeen = true
				state.turnCompletionSuccess = true
				msg := ""
				if len(event.Data) > 0 {
					taskData, dataErr := parseSessionTaskCompleteData(event.Data)
					if dataErr == nil {
						state.turnCompletionSummary = taskData.Summary
						if taskData.Success != nil {
							state.turnCompletionSuccess = *taskData.Success
						}
						msg = taskData.Summary
					}
				}
				agentcore.EmitNotification(emit, msg)

			case "session.mcp_server_status_changed", "session.mcp_servers_loaded",
				"session.tools_updated":
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

			case "session.error":
				if len(event.Data) > 0 {
					errData, dataErr := parseSessionErrorData(event.Data)
					if dataErr == nil {
						state.turnErrorMessage = strings.TrimSpace(errData.Message)
					}
				}
				agentcore.EmitNotification(emit, "")

			case "user.message":
				if len(event.Data) > 0 {
					msgData, dataErr := parseUserMessageData(event.Data)
					if dataErr == nil && msgData.IsAutopilotContinuation {
						state.turnContinuations++
					}
				}
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

			case "result":
				return new(event), nil

			default:
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))
				agentcore.EmitNotification(emit, "")
			}

			return nil, nil
		},
		GetUsage:     func() (domain.TokenUsage, bool) { return state.usage.Snapshot(), state.usage.Measured() },
		GetSessionID: func() string { return state.copilotSessionID },
		OnFinalize: func(emit func(domain.AgentEvent), lastParsed any, exitCode int, waitErr error, stderrLines []string, earlyExit *domain.AgentError, spend *agentcore.TurnSpend) (domain.TurnResult, *domain.AgentError) {
			lastResult, _ := lastParsed.(*rawEvent)

			// Capture session ID from result event for subsequent turns.
			if lastResult != nil && lastResult.SessionID != "" {
				state.copilotSessionID = lastResult.SessionID
				state.refreshForkLogger()
			}

			recovered, estimated, resume := state.recoverUsage(state.logger())
			if lastResult != nil && recovered != nil {
				spend.MarkSettled()
				spend.MarkSessionTotal()
			}
			if estimated {
				spend.MarkEstimated()
			}

			ev := agentcore.TurnEvidence{
				ExitObserved: true,
				ExitCode:     exitCode,
				WaitErr:      waitErr,
				EarlyExit:    earlyExit,
			}
			ev.Work, ev.WorkDetail = state.work.Report()

			var apiDurationMS int64
			if lastResult != nil {
				if lastResult.Usage != nil {
					apiDurationMS = lastResult.Usage.TotalAPIDurMS
					state.logger().Info("copilot turn completed",
						slog.Int64("premium_requests", lastResult.Usage.PremiumRequests))
				}

				switch {
				case lastResult.ExitCode == nil || *lastResult.ExitCode != 0:
					ev.Terminal = agentcore.TerminalFailure
					ev.TerminalMessage = "non-zero exit in result event"
					if state.turnErrorMessage != "" {
						ev.TerminalMessage = state.turnErrorMessage
					}
				case !state.turnCompletionSeen:
					ev.Terminal = agentcore.TerminalIncomplete
					ev.TerminalMessage = "raise copilot-cli.max_autopilot_continues if the turn needs more steps"
					state.logger().Warn("copilot turn ended without a task-completion report",
						slog.Int("autopilot_continuations_observed", state.turnContinuations),
						slog.Int("max_autopilot_continues", effectiveMaxAutopilotContinues(state.passthrough)))
				case !state.turnCompletionSuccess:
					ev.Terminal = agentcore.TerminalFailure
					ev.TerminalErrorKind = domain.ErrTurnFailed
					ev.TerminalMessage = completionFailureMessage(state.turnCompletionSummary)
				default:
					ev.Terminal = agentcore.TerminalSuccess
				}
			}

			result, agentErr := state.usage.Finalize(emit, state.logger(), ev, state.copilotSessionID, apiDurationMS, recovered, *spend)
			result.ResumeUsage = resume
			return result, agentErr
		},
		// Copilot emits EventSessionStarted before the scan loop using the
		// session's own ID, assigned in StartSession.
		EmitSessionStartID: func() string { return state.copilotSessionID },
	}

	state.forkSession = agentcore.NewForkPerTurnSession(&state.target, hooks, state.logger(), state.agentConfig.StopGraceMS)

	return domain.Session{
		ID:       copilotSessionID,
		Internal: state,
	}, nil
}

// RunTurn executes one agent turn by delegating to the session's
// [agentcore.ForkPerTurnSession]. Per-turn scan state is reset before
// delegation so each turn starts with a fresh accumulator and clean state.
func (a *CopilotAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("copilot: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, fmt.Errorf("unexpected session internal type %T", session.Internal)
	}

	state.refreshForkLogger()

	// Reset per-turn scan state before delegation. state.usage is
	// constructed once in StartSession and carries the run-cumulative
	// snapshot across turns, so it is not reset here.
	state.inFlight = agentcore.NewToolTracker()
	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true})
	state.turnCompletionSeen = false
	state.turnCompletionSuccess = false
	state.turnCompletionSummary = ""
	state.turnErrorMessage = ""
	state.turnContinuations = 0

	return state.forkSession.RunTurn(ctx, params.Prompt, params.OnEvent)
}

// StopSession terminates a running Copilot CLI subprocess gracefully by
// delegating to the session's [agentcore.ForkPerTurnSession].
func (a *CopilotAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	var stopErr error
	if state.forkSession != nil {
		stopErr = state.forkSession.Stop(ctx)
	}
	if state.credentialVerification && state.copilotSessionID != "" {
		deleteVerificationSession(ctx, state.target, state.copilotSessionID, agentcore.AuxiliaryTimeout(state.agentConfig), state.agentConfig.StopGraceMS, state.logger())
	}
	return stopErr
}
