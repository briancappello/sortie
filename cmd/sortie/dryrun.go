package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// runDryRun executes a single poll cycle in read-only mode: fetches
// candidate issues, computes dispatch eligibility, logs results, and
// returns an exit code. No database is opened, no agents are spawned,
// and no state is written. The caller constructs and defers closing
// the tracker adapter.
//
// resolver may be nil, in which case no candidate's blockers are
// resolved. A single dry-run cycle spends at most
// orchestrator's per-pass blocker-read budget, evaluated against one
// [orchestrator.TickResolution] constructed at offset zero for the
// whole cycle.
func runDryRun(ctx context.Context, cfg config.ServiceConfig, logger *slog.Logger, trackerAdapter domain.TrackerAdapter, resolver orchestrator.BlockerResolver, satisfaction orchestrator.PlanSatisfactionLoader) int {
	wc := orchestrator.ParseWorkerConfig(cfg.ExtensionSection("worker"), cfg.ExtensionEnvRefPaths("worker"))
	hostPool := orchestrator.NewHostPool(wc.SSHHosts, wc.MaxPerHost)

	orchestrator.LogAdvisories(ctx, logger, append(cfg.Advisories(), wc.Warnings...))

	issues, err := trackerAdapter.FetchCandidateIssues(ctx)
	if err != nil {
		logger.Error("dry-run: failed to fetch candidate issues", slog.Any("error", err))
		return 1
	}

	sorted := orchestrator.SortForDispatch(issues)

	state := orchestrator.NewState(
		cfg.Polling.IntervalMS,
		cfg.Agent.MaxConcurrentAgents,
		cfg.Agent.MaxTokens,
		cfg.Agent.MaxConcurrentByState,
		orchestrator.AgentTotals{},
	)
	state.MaxCostUSD = cfg.Agent.MaxCostUSD

	activeSet := dryRunStateSet(cfg.Tracker.ActiveStates)
	terminalSet := dryRunStateSet(cfg.Tracker.TerminalStates)

	state.BlockerJudge = orchestrator.PlanJudgeFor(ctx, cfg.Tracker, satisfaction, sorted, logger)

	pass := &orchestrator.TickResolution{}

	var eligible, ineligible int
	for i, issue := range sorted {
		globalAvail := orchestrator.GlobalAvailableSlots(
			state.MaxConcurrentAgents, len(state.Running))

		if hostPool.IsSSHEnabled() && !hostPool.HasCapacity() {
			for _, remaining := range sorted[i:] {
				ineligible++
				logger.Info("dry-run: candidate",
					slog.String("issue_id", remaining.ID),
					slog.String("issue_identifier", remaining.Identifier),
					slog.String("state", remaining.State),
					slog.Bool("would_dispatch", false),
					slog.String("skip_reason", "ssh_hosts_at_capacity"),
				)
			}
			break
		}

		stateRunning := orchestrator.RunningCountByState(state.Running, issue.State)
		stateAvail := orchestrator.StateAvailableSlots(
			issue.State, state.MaxConcurrentByState, stateRunning, globalAvail)

		// Capacity is evaluated before EvaluateCandidate so a
		// candidate with no free slot spends no blocker read.
		wouldDispatch := false
		skipReason := ""
		if globalAvail > 0 && stateAvail > 0 {
			decision := orchestrator.EvaluateCandidate(ctx, issue, state, activeSet, terminalSet, resolver, pass)
			issue = decision.Issue
			wouldDispatch = decision.Dispatch
			if isBlockerSkipReason(decision.Reason) {
				skipReason = string(decision.Reason)
			}
		}

		if wouldDispatch && hostPool.IsSSHEnabled() {
			_, ok := hostPool.AcquireHost(issue.ID, "")
			if !ok {
				wouldDispatch = false
				skipReason = "ssh_hosts_at_capacity"
			}
		}

		logFields := []any{
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
			slog.String("title", issue.Title),
			slog.String("state", issue.State),
			slog.Bool("would_dispatch", wouldDispatch),
			slog.Int("global_slots_available", globalAvail),
			slog.Int("state_slots_available", stateAvail),
		}
		if issue.Priority != nil {
			logFields = append(logFields, slog.Int("priority", *issue.Priority))
		}
		if hostPool.IsSSHEnabled() {
			logFields = append(logFields, slog.String("ssh_host", hostPool.HostFor(issue.ID)))
		}
		if skipReason != "" {
			logFields = append(logFields, slog.String("skip_reason", skipReason))
		}

		logger.Info("dry-run: candidate", logFields...)

		if wouldDispatch {
			eligible++
			state.Claimed[issue.ID] = struct{}{}
			state.Running[issue.ID] = &orchestrator.RunningEntry{
				Identifier: issue.Identifier,
				Issue:      issue,
			}
		} else {
			ineligible++
		}
	}

	logger.Info("dry-run: complete",
		slog.Int("candidates_fetched", len(issues)),
		slog.Int("would_dispatch", eligible),
		slog.Int("ineligible", ineligible),
		slog.Int("max_concurrent_agents", cfg.Agent.MaxConcurrentAgents),
	)

	return 0
}

// isBlockerSkipReason reports whether reason is one of the four
// blocker-hold reasons [orchestrator.EvaluateCandidate] can name, as
// opposed to [orchestrator.SkipIneligible] or [orchestrator.SkipNone].
func isBlockerSkipReason(reason orchestrator.SkipReason) bool {
	switch reason {
	case orchestrator.SkipBlockedBy, orchestrator.SkipBlockersUnresolved,
		orchestrator.SkipBlockersNotRead, orchestrator.SkipBlockersIncomplete:
		return true
	default:
		return false
	}
}

// dryRunStateSet builds a set of lowercase state names for O(1) membership
// testing. Mirrors orchestrator.stateSet which is unexported.
func dryRunStateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, s := range states {
		set[strings.ToLower(s)] = struct{}{}
	}
	return set
}

// dryRunSatisfaction opens the database read-only so a dry run judges
// plan blockers as the service would. It returns nil when plan control is
// off or the database cannot be opened, and then a dry run reports every
// plan-keyed blocker as unsatisfied.
func dryRunSatisfaction(ctx context.Context, cfg config.ServiceConfig, workflowPath string, logger *slog.Logger) orchestrator.PlanSatisfactionLoader {
	if !cfg.Tracker.PlanControl() {
		return nil
	}
	dbPath := resolveDBPath(cfg.DBPath, filepath.Dir(workflowPath))
	store, err := persistence.OpenReadOnly(ctx, dbPath)
	if err != nil {
		logger.Warn("dry-run: cannot read plan satisfaction, plan-keyed blockers count as unsatisfied",
			slog.String("db_path", dbPath), slog.Any("error", err))
		return nil
	}
	return store
}
