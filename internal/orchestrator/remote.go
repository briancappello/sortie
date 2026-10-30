package orchestrator

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/jobtoken"
	"github.com/sortie-ai/sortie/internal/worker"
)

// leaseSweepInterval is how often the orchestrator ends attempts whose
// worker lease expired.
const leaseSweepInterval = time.Second

// remoteWorkers holds the matcher the remote environment kind hands
// runs to, the lease rule it uses, and the issuer of its job tokens.
type remoteWorkers struct {
	leaser  *worker.TTLLeaser
	matcher *worker.Matcher
	tokens  *jobtoken.Issuer
}

func newRemoteWorkers(now func() time.Time) *remoteWorkers {
	leaser := worker.NewTTLLeaser(time.Duration(60000) * time.Millisecond)
	tokens := jobtoken.NewIssuer()
	return &remoteWorkers{leaser: leaser, tokens: tokens,
		matcher: worker.NewMatcher(leaser, now, jobTokens{tokens})}
}

// jobTokens gives the matcher the issuer's tokens: one per flow of the
// job, bound to its attempt, minted at the lease grant and revoked when
// the attempt ends.
type jobTokens struct{ issuer *jobtoken.Issuer }

func (t jobTokens) Mint(j worker.Job) (map[string]string, error) {
	flows := make([]jobtoken.Flow, 0, len(j.Flows))
	for f := range j.Flows {
		flows = append(flows, jobtoken.Flow(f))
	}
	toks, err := t.issuer.Mint(j.ID, uint64(max(j.Attempt, 0)), flows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(toks))
	for f, tok := range toks {
		out[string(f)] = tok.Reveal()
	}
	return out, nil
}

func (t jobTokens) Void(jobID string) { t.issuer.OnLeaseEnd(jobID) }

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

// JobTokens returns the issuer of job tokens while the current
// configuration selects the remote environment kind, and nil otherwise.
func (o *Orchestrator) JobTokens() *jobtoken.Issuer {
	if o.Workers() == nil {
		return nil
	}
	return o.remote.tokens
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

// credentialPlaceholder is what the machine holds in place of the
// credential name; the egress proxy replaces it on the credential's
// hosts.
func credentialPlaceholder(name string) string { return "sortie-credential-" + name }

// remoteJob builds the matcher job of one dispatched attempt of issue.
// Its ID is the issue identifier and its attempt the run history's,
// counting from 1. Its run descriptor holds no credential: Sortie reads
// no credential value under the remote kind, and the machine gets each
// credential the workflow binds only as a placeholder.
func remoteJob(ctx context.Context, cfg config.ServiceConfig, issue domain.Issue, attempt *int, agentKind, defaultCommand string) worker.Job {
	rc := cfg.Environment.Remote
	n := normalizeAttempt(attempt) + 1
	caps := slices.Clone(rc.Requires.Capabilities)
	if rc.Backend != "" && !slices.Contains(caps, rc.Backend) {
		caps = append(caps, rc.Backend)
	}
	if caps == nil {
		caps = []string{}
	}
	identity := sessionIdentityFrom(ctx)
	identity.Token = ""
	env := map[string]string{}
	for _, v := range buildSessionEnv(sessionEnvParams{
		Issue: issue, DispatchID: dispatchIDFromContext(ctx), Attempt: attempt, Identity: identity,
		WorkspacePath: remoteWorkspace, Purpose: sessionPurposeWork,
	}) {
		if v.Name != envSessionAPIURL && v.Name != envSessionToken {
			env[v.Name] = v.Value
		}
	}
	creds := make([]worker.RunCredential, 0, len(rc.Credentials))
	for _, c := range rc.Credentials {
		env[c.Env] = credentialPlaceholder(c.Name)
		creds = append(creds, worker.RunCredential{Name: c.Name, Env: c.Env, Placeholder: credentialPlaceholder(c.Name),
			Hosts: slices.Clone(c.Hosts), Header: c.Header})
	}
	command, args := remoteAgentCommand(cfg.AgentCommand(agentKind, false), defaultCommand)
	return worker.Job{
		ID:       issue.Identifier,
		Attempt:  n,
		Priority: issue.Priority,
		Issue:    worker.Issue{ID: issue.ID, Identifier: issue.Identifier, Title: issue.Title},
		Requires: worker.Requires{Capabilities: caps, CPU: rc.Requires.CPU, MemGiB: rc.Requires.MemGiB, DiskGiB: rc.Requires.DiskGiB},
		Flows:    maps.Clone(rc.Flows),
		Run: worker.Run{
			Backend:        rc.Backend,
			Image:          rc.Image,
			Command:        command,
			Args:           args,
			Env:            env,
			StallTimeoutMS: cfg.Agent.StallTimeoutMS,
			SeedPath:       worker.SeedPath(issue.Identifier, n),
			Credentials:    creds,
		},
	}
}

// remoteWorkspace is the workspace path inside a remote job's machine.
const remoteWorkspace = "/workspace"

// remoteAgentCommand splits the agent command into the program and its
// arguments: a list as written, a string on whitespace, and the kind's
// default command when the workflow sets none.
func remoteAgentCommand(c domain.AgentCommand, defaultCommand string) (string, []string) {
	argv := slices.Clone(c.Argv)
	if len(argv) == 0 {
		line := c.Line
		if strings.TrimSpace(line) == "" {
			line = defaultCommand
		}
		argv = strings.Fields(line)
	}
	if len(argv) == 0 {
		return "", []string{}
	}
	return argv[0], append([]string{}, argv[1:]...)
}

// RunRemoteAttempt hands one attempt of issue to the matcher and waits
// for it to end: by the worker's report, by its lease expiring, or by
// ctx. Nothing of the issue runs on this host. defaultCommand is the
// agent kind's own command, used when the workflow sets none. onExit is called exactly
// once with the result, which the orchestrator handles as any worker
// exit, retries included.
func RunRemoteAttempt(ctx context.Context, m *worker.Matcher, cfg config.ServiceConfig, issue domain.Issue, attempt *int, agentKind, defaultCommand string, onExit func(issueID string, result WorkerResult)) {
	job := remoteJob(ctx, cfg, issue, attempt, agentKind, defaultCommand)
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
			msg := out.Reason
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
