package orchestrator

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/worker"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// leaseSweepInterval is how often the orchestrator ends attempts whose
// worker lease expired.
const leaseSweepInterval = time.Second

// remoteWorkers holds the matcher the remote environment kind hands
// runs to, and the lease rule it uses.
type remoteWorkers struct {
	leaser  *worker.TTLLeaser
	matcher *worker.Matcher
}

func newRemoteWorkers(now func() time.Time, mint worker.TokenMinter) *remoteWorkers {
	leaser := worker.NewTTLLeaser(time.Duration(60000) * time.Millisecond)
	return &remoteWorkers{leaser: leaser, matcher: worker.NewMatcher(leaser, now, mint)}
}

// applyConfig takes the lease duration of cfg's remote block.
func (r *remoteWorkers) applyConfig(cfg config.EnvironmentConfig) {
	if r != nil && isRemoteKind(cfg) && cfg.Remote.LeaseTTLMS > 0 {
		r.leaser.SetTTL(time.Duration(cfg.Remote.LeaseTTLMS) * time.Millisecond)
	}
}

func isRemoteKind(cfg config.EnvironmentConfig) bool {
	return cfg.Kind == config.EnvironmentKindRemote
}

// Workers returns the worker matcher while the current configuration
// selects the remote environment kind, and nil otherwise, so the worker
// API is served under the remote kind only.
func (o *Orchestrator) Workers() *worker.Matcher {
	if o.remote == nil || !isRemoteKind(o.workflowManager.Config().Environment) {
		return nil
	}
	return o.remote.matcher
}

// WorkerIdentityHeader returns worker.identity_header of the current
// configuration.
func (o *Orchestrator) WorkerIdentityHeader() string {
	if h := o.workflowManager.Config().Environment.Remote.IdentityHeader; h != "" {
		return h
	}
	return config.DefaultWorkerIdentityHeader
}

// expireLeases ends every attempt whose lease expired. The attempt's
// worker goroutine receives the outcome and reports the exit, which
// takes the normal retry path.
func (o *Orchestrator) expireLeases() {
	if o.remote != nil {
		o.remote.matcher.ExpireLeases()
	}
}

// remoteJob builds the matcher job of one dispatched attempt of issue.
func remoteJob(ctx context.Context, cfg config.ServiceConfig, issue domain.Issue, attempt *int, agentKind string) worker.Job {
	rc := cfg.Environment.Remote
	n := normalizeAttempt(attempt)
	id := dispatchIDFromContext(ctx)
	if id == "" {
		id = rand.Text()
	}
	key, _ := workspace.SanitizeKey(issue.Identifier)
	return worker.Job{
		ID:         id,
		Identifier: issue.Identifier,
		Attempt:    n,
		Priority:   issue.Priority,
		Requires:   append([]string{}, rc.Requires...),
		Needs:      worker.Resources{CPUs: rc.CPUs, MemoryMiB: int64(rc.MemoryMiB), DiskMiB: int64(rc.DiskMiB)},
		Flows:      append([]string{}, rc.Flows...),
		Run: worker.Run{
			IssueID:      issue.ID,
			Identifier:   issue.Identifier,
			Title:        issue.Title,
			URL:          issue.URL,
			Attempt:      n,
			AgentKind:    agentKind,
			WorkspaceKey: key,
		},
	}
}

// RunRemoteAttempt hands one attempt of issue to the matcher and waits
// for it to end: by the worker's report, by its lease expiring, or by
// ctx. Nothing of the issue runs on this host. onExit is called exactly
// once with the result, which the orchestrator handles as any worker
// exit, retries included.
func RunRemoteAttempt(ctx context.Context, m *worker.Matcher, cfg config.ServiceConfig, issue domain.Issue, attempt *int, agentKind string, onExit func(issueID string, result WorkerResult)) {
	job := remoteJob(ctx, cfg, issue, attempt, agentKind)
	ended := m.Submit(job)
	// The run spends nothing on this host. Protocol 1 reports no token
	// usage, so the run's figures are the zero a run without a turn has.
	var (
		localUsage        domain.TokenUsage
		localMeasured     = true
		localModelName    string
		localRequestCount int
		localUnaccounted  int
	)
	result := func(kind WorkerExitKind, err error) WorkerResult {
		r := WorkerResult{
			IssueID:          issue.ID,
			Identifier:       issue.Identifier,
			ExitKind:         WorkerExitError,
			Error:            err,
			AgentAdapter:     agentKind,
			Attempt:          attempt,
			Usage:            localUsage,
			UsageMeasured:    localMeasured,
			ModelName:        localModelName,
			APIRequestCount:  localRequestCount,
			UnaccountedTurns: localUnaccounted,
		}
		r.ExitKind = kind
		if kind == WorkerExitCancelled {
			r.CancelCause = context.Cause(ctx)
			r.StoppedByCeiling = errors.Is(r.CancelCause, errCeilingStop)
		}
		return r
	}
	select {
	case out := <-ended:
		switch {
		case out.Succeeded:
			onExit(issue.ID, result(WorkerExitNormal, nil))
		case out.Cancelled:
			onExit(issue.ID, result(WorkerExitCancelled, nil))
		default:
			msg := out.Error
			if msg == "" {
				msg = "worker reported failure"
			}
			onExit(issue.ID, result(WorkerExitError, errors.New(msg)))
		}
	case <-ctx.Done():
		m.Cancel(job.ID)
		onExit(issue.ID, result(WorkerExitCancelled, nil))
	}
}
