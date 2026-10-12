package orchestrator

import (
	"context"
	"strconv"

	"github.com/sortie-ai/sortie/internal/domain"
)

// Names of the session identity variables every agent launch receives.
const (
	envSessionIssueID         = "SORTIE_ISSUE_ID"
	envSessionIssueIdentifier = "SORTIE_ISSUE_IDENTIFIER"
	envSessionDispatchID      = "SORTIE_DISPATCH_ID"
	envSessionAttempt         = "SORTIE_ATTEMPT"
	envSessionRule            = "SORTIE_RULE"
	envSessionChainID         = "SORTIE_CHAIN_ID"
	envSessionWorkspace       = "SORTIE_WORKSPACE"
	envSessionAPIURL          = "SORTIE_API_URL"
	envSessionPurpose         = "SORTIE_SESSION_PURPOSE"
	envSessionToken           = "SORTIE_SESSION_TOKEN" //nolint:gosec // a variable name, not a credential
)

// Values of [envSessionPurpose].
const (
	sessionPurposeWork            = "work"
	sessionPurposeCredentialCheck = "credential-check" //nolint:gosec // a purpose label, not a credential
)

// sessionIdentity is the part of a run's identity the dispatch site
// knows before the worker starts and the worker cannot read from the
// running entry without racing the event loop.
type sessionIdentity struct {
	RuleName  string
	ChainID   string
	Token     string
	TurnClock *turnClock
}

type sessionIdentityCtxKey struct{}

// withSessionIdentity returns a child context carrying id for the
// worker the dispatch is about to start.
func withSessionIdentity(ctx context.Context, id sessionIdentity) context.Context {
	return context.WithValue(ctx, sessionIdentityCtxKey{}, id)
}

// sessionIdentityFrom extracts the value injected by
// [withSessionIdentity], or the zero value when absent.
func sessionIdentityFrom(ctx context.Context) sessionIdentity {
	id, _ := ctx.Value(sessionIdentityCtxKey{}).(sessionIdentity)
	return id
}

// sessionEnvParams holds the inputs of [buildSessionEnv].
type sessionEnvParams struct {
	Issue         domain.Issue
	DispatchID    string
	Attempt       *int
	Identity      sessionIdentity
	WorkspacePath string
	APIURL        string
	Purpose       string
}

// buildSessionEnv returns the session identity variables for one
// launch. The attempt is empty for a first dispatch and the retry
// number otherwise, matching the tool server's SORTIE_ATTEMPT. The
// session token is set on a work launch only.
func buildSessionEnv(p sessionEnvParams) []domain.EnvVar {
	attempt := ""
	if p.Attempt != nil {
		attempt = strconv.Itoa(*p.Attempt)
	}
	env := []domain.EnvVar{
		{Name: envSessionIssueID, Value: p.Issue.ID},
		{Name: envSessionIssueIdentifier, Value: p.Issue.Identifier},
		{Name: envSessionDispatchID, Value: p.DispatchID},
		{Name: envSessionAttempt, Value: attempt},
		{Name: envSessionRule, Value: p.Identity.RuleName},
		{Name: envSessionChainID, Value: p.Identity.ChainID},
		{Name: envSessionWorkspace, Value: p.WorkspacePath},
		{Name: envSessionAPIURL, Value: p.APIURL},
		{Name: envSessionPurpose, Value: p.Purpose},
	}
	if p.Purpose == sessionPurposeWork && p.Identity.Token != "" {
		env = append(env, domain.EnvVar{Name: envSessionToken, Value: p.Identity.Token})
	}
	return env
}
