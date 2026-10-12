// Package huly implements [domain.TrackerAdapter] against a Huly
// workspace (https://huly.io), self-hosted or cloud. It reads and writes
// through the transactor's REST API and reads issue descriptions from the
// collaborator service, converting Huly's rich-text markup to Markdown.
// Registered under kind "huly" via an init function.
package huly

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("huly", NewHulyAdapter, registry.TrackerMeta{
		RequiresProject:       true,
		RequiresAPIKey:        true,
		ValidateTrackerConfig: validateConfig,
		BlockerSource:         registry.BlockersFromCandidates,
		WriteCapabilities:     registry.AllWriteCapabilities(),
		PlanKeyStorage:        registry.PlanKeyCustomField,
		Implementation:        (*HulyAdapter)(nil),
	})
}

// Compile-time interface satisfaction checks.
var (
	_ domain.TrackerAdapter   = (*HulyAdapter)(nil)
	_ domain.PlanKeyFinder    = (*HulyAdapter)(nil)
	_ domain.IssueCreator     = (*HulyAdapter)(nil)
	_ domain.IssueUpdater     = (*HulyAdapter)(nil)
	_ domain.IssueParenter    = (*HulyAdapter)(nil)
	_ domain.DependencyLinker = (*HulyAdapter)(nil)
)

// HulyAdapter implements [domain.TrackerAdapter] against one Huly project.
// Every field except the caches and metrics is set at construction and
// never mutated. Safe for concurrent use.
type HulyAdapter struct {
	rpc          rpcClient
	project      hulyProject
	socialID     string
	issueURLBase string

	// StateSource supplies the workflow's current state lists.
	*issuekit.StateSource
	log *slog.Logger

	// projectStatuses maps a lowercased status name of the project's
	// workflow to its status ids.
	projectStatuses map[string][]string

	// plan maps the planning kinds to the project's task types.
	plan planTypes

	// attrMu guards attrsReady, set once the plan attributes exist.
	attrMu     sync.Mutex
	attrsReady bool

	mu       sync.RWMutex
	statuses map[string]hulyStatus // status id -> status, any project
	metrics  domain.Metrics
}

// NewHulyAdapter creates a [HulyAdapter] from adapter configuration.
//
// Required config keys:
//   - "endpoint": the Huly front URL, for example "http://localhost:8087".
//   - "api_key": a Huly token, either a login token or an API token.
//   - "project": the project identifier, the prefix of issue keys such as
//     "HULY" in "HULY-12".
//
// Optional config keys, read from the "huly" extension section:
//   - "workspace": the workspace URL name. It may be omitted when the
//     token is already scoped to one workspace.
//   - "epic_type", "story_type": the project task types that the planning
//     kinds epic and story map to. Plan writes need both.
//
// The constructor logs in, resolves the project, its workflow states and
// the configured task types, and fails when a configured state or task
// type is absent from the project.
func NewHulyAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	fields := map[string]string{}
	for _, key := range []string{"endpoint", "api_key", "project", "workspace", "user_agent", "handoff_state", "in_progress_state", "epic_type", "story_type"} {
		value, fault := typeutil.StringField(config, key)
		if fault != nil {
			return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
		}
		fields[key] = strings.TrimSpace(value)
	}

	if fields["api_key"] == "" {
		return nil, &domain.TrackerError{Kind: domain.ErrMissingTrackerAPIKey, Message: "missing required config key: api_key"}
	}
	if fields["project"] == "" {
		return nil, &domain.TrackerError{Kind: domain.ErrMissingTrackerProject, Message: "missing required config key: project"}
	}
	if _, ok := parseEndpoint(fields["endpoint"]); !ok {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "huly: tracker.endpoint must be the absolute http(s) URL of the Huly front, for example http://localhost:8087",
		}
	}

	userAgent := fields["user_agent"]
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	states, err := issuekit.TrackerStatesFromConfig(config)
	if err != nil {
		return nil, err
	}
	active, terminal := states.Active, states.Terminal

	ctx := context.Background()
	conn, err := connect(ctx, fields["endpoint"], fields["api_key"], fields["workspace"], userAgent)
	if err != nil {
		return nil, err
	}

	issueURLBase := fmt.Sprintf("%s/workbench/%s/tracker/", conn.frontBaseURL, url.PathEscape(conn.login.WorkspaceURL))

	configured := append(append([]string{}, active...), terminal...)
	for _, key := range []string{"handoff_state", "in_progress_state"} {
		if fields[key] != "" {
			configured = append(configured, fields[key])
		}
	}

	a, err := newAdapter(ctx, conn.rpc, fields["project"], conn.socialID, issueURLBase, active, configured, slog.Default())
	if err != nil {
		return nil, err
	}
	if err := a.resolvePlanTypes(ctx, fields["epic_type"], fields["story_type"]); err != nil {
		return nil, err
	}
	return a, nil
}

// newAdapter resolves the project and its workflow states through rpc and
// assembles the adapter. It is the seam shared by the production
// constructor and offline tests.
func newAdapter(ctx context.Context, rpc rpcClient, projectIdentifier, socialID, issueURLBase string, active, configured []string, log *slog.Logger) (*HulyAdapter, error) {
	raws, err := rpc.FindAll(ctx, classProject, map[string]any{"identifier": idIn([]string{projectIdentifier})}, map[string]any{"limit": 1})
	if err != nil {
		return nil, err
	}
	projects, err := decodeAll[hulyProject](raws)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("huly: project %q not found in the workspace", projectIdentifier),
		}
	}
	project := projects[0]

	a := &HulyAdapter{
		rpc:             rpc,
		project:         project,
		socialID:        socialID,
		issueURLBase:    issueURLBase,
		log:             log,
		projectStatuses: map[string][]string{},
		statuses:        map[string]hulyStatus{},
	}

	if err := a.loadProjectStatuses(ctx); err != nil {
		return nil, err
	}

	var missing []string
	for _, name := range configured {
		if _, ok := a.projectStatuses[strings.ToLower(strings.TrimSpace(name))]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, &domain.TrackerError{
			Kind: domain.ErrTrackerPayload,
			Message: fmt.Sprintf("huly: state(s) %s not in project %s; its states are %s",
				strings.Join(missing, ", "), project.Identifier, strings.Join(a.projectStatusNames(), ", ")),
		}
	}

	a.StateSource = issuekit.NewStateSource(domain.TrackerStates{Active: active})
	return a, nil
}

// loadProjectStatuses reads the statuses of the project's workflow.
func (a *HulyAdapter) loadProjectStatuses(ctx context.Context) error {
	raws, err := a.rpc.FindAll(ctx, classProjectType, map[string]any{"_id": idIn([]string{a.project.Type})}, map[string]any{"limit": 1})
	if err != nil {
		return err
	}
	types, err := decodeAll[hulyProjectType](raws)
	if err != nil {
		return err
	}
	if len(types) == 0 {
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("huly: project type %q of project %s not found", a.project.Type, a.project.Identifier),
		}
	}

	ids := make([]string, 0, len(types[0].Statuses))
	for _, s := range types[0].Statuses {
		ids = append(ids, s.ID)
	}

	statuses, err := a.lookupStatuses(ctx, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if s, ok := statuses[id]; ok {
			key := strings.ToLower(s.Name)
			a.projectStatuses[key] = append(a.projectStatuses[key], s.ID)
		}
	}
	return nil
}

func (a *HulyAdapter) projectStatusNames() []string {
	seen := map[string]bool{}
	var names []string

	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, ids := range a.projectStatuses {
		for _, id := range ids {
			if s, ok := a.statuses[id]; ok && !seen[s.Name] {
				seen[s.Name] = true
				names = append(names, s.Name)
			}
		}
	}
	return names
}

// lookupStatuses returns the statuses with the given ids, reading the ones
// not cached yet.
func (a *HulyAdapter) lookupStatuses(ctx context.Context, ids []string) (map[string]hulyStatus, error) {
	out := make(map[string]hulyStatus, len(ids))
	var missing []string

	a.mu.RLock()
	for _, id := range ids {
		if s, ok := a.statuses[id]; ok {
			out[id] = s
		} else if id != "" {
			missing = append(missing, id)
		}
	}
	a.mu.RUnlock()

	if len(missing) == 0 {
		return out, nil
	}

	raws, err := a.rpc.FindAll(ctx, classIssueStatus, map[string]any{"_id": map[string]any{"$in": dedupe(missing)}}, nil)
	if err != nil {
		return nil, err
	}
	statuses, err := decodeAll[hulyStatus](raws)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range statuses {
		a.statuses[s.ID] = s
		out[s.ID] = s
	}
	return out, nil
}

// statusIDs returns the ids of the project statuses named by names,
// compared case-insensitively. Names the project lacks contribute nothing.
func (a *HulyAdapter) statusIDs(names []string) []string {
	var ids []string
	for _, name := range names {
		ids = append(ids, a.projectStatuses[strings.ToLower(strings.TrimSpace(name))]...)
	}
	return ids
}

// SetMetrics installs a metrics recorder for tracker API calls.
func (a *HulyAdapter) SetMetrics(m domain.Metrics) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.metrics = m
}

func (a *HulyAdapter) recorder() domain.Metrics {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.metrics
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func idIn(ids []string) map[string]any {
	return map[string]any{"$in": ids}
}
