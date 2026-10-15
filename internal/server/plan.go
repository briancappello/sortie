package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/plan"
	"github.com/sortie-ai/sortie/internal/registry"
)

// maxPlanBodyBytes caps a plan sync request: a graph document is larger
// than the other control bodies.
const maxPlanBodyBytes = 8 << 20

// planApplyWriteTimeout replaces the server's write timeout for an apply,
// which makes one tracker request per write.
const planApplyWriteTimeout = 10 * time.Minute

// planSyncRequest is the POST /api/v1/plan/sync body.
type planSyncRequest struct {
	Graph     json.RawMessage `json:"graph"`
	Apply     bool            `json:"apply"`
	PreviewID string          `json:"preview_id"`
}

type planPreviewResponse struct {
	PreviewID    string                     `json:"preview_id"`
	Preview      plan.Preview               `json:"preview"`
	Capabilities []registry.WriteCapability `json:"capabilities"`
}

type planApplyResponse struct {
	Result plan.Result  `json:"result"`
	Error  *errorDetail `json:"error"`
}

type planCapabilitiesResponse struct {
	Capabilities   []registry.WriteCapability `json:"capabilities"`
	PlanKeyStorage *string                    `json:"plan_key_storage"`
}

// planGraphResponse is GET /api/v1/plan/graph. Every field is present; a
// value the tracker cannot supply is null.
type planGraphResponse struct {
	// Project is the tracker project the graph comes from. Dispatched
	// reports whether Sortie dispatches agents for it: runtime state is
	// null on every node of a project it does not dispatch.
	Project      string                     `json:"project"`
	Dispatched   bool                       `json:"dispatched"`
	GeneratedAt  time.Time                  `json:"generated_at"`
	Capabilities []registry.WriteCapability `json:"capabilities"`
	Nodes        []planGraphNode            `json:"nodes"`
	Edges        []planGraphEdge            `json:"edges"`
}

type planGraphNode struct {
	ID         string       `json:"id"`
	Identifier string       `json:"identifier"`
	PlanKey    *string      `json:"plan_key"`
	Kind       *string      `json:"kind"`
	Title      string       `json:"title"`
	State      string       `json:"state"`
	Terminal   bool         `json:"terminal"`
	URL        *string      `json:"url"`
	ParentID   *string      `json:"parent_id"`
	Runtime    *planRuntime `json:"runtime"`
}

// planRuntime is a node's Sortie runtime state.
type planRuntime struct {
	Status      string  `json:"status"`
	TurnCount   *int    `json:"turn_count"`
	LastMessage *string `json:"last_message"`
	Attempt     *int    `json:"attempt"`
}

type planGraphEdge struct {
	BlockerID string `json:"blocker_id"`
	BlockedID string `json:"blocked_id"`
}

// controlGet wraps a read handler with GET method checking and the
// control authentication of [Server.requireControl].
func (s *Server) controlGet(next http.HandlerFunc) http.HandlerFunc {
	guarded := s.requireControl(scopeOperator, next)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			s.methodNotAllowed(w, r, http.MethodGet)
			return
		}
		guarded(w, r)
	}
}

// planSyncerFor returns the syncer of the project the request's
// "project" query parameter names, the configured project when it names
// none. It writes the error response and reports false when there is no
// such syncer.
func (s *Server) planSyncerFor(w http.ResponseWriter, r *http.Request) (*plan.Syncer, bool) {
	if s.planProjects == nil {
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable, "plan_unavailable", "plan sync is not configured")
		return nil, false
	}
	syncer, err := s.planProjects.Syncer(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		s.writePlanProjectError(w, r, err)
		return nil, false
	}
	return syncer, true
}

func (s *Server) writePlanProjectError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, plan.ErrProjectNotFound):
		writeErrorJSON(w, s.logger, http.StatusNotFound, "project_not_found", err.Error())
	case errors.Is(err, plan.ErrProjectsUnsupported):
		writeErrorJSON(w, s.logger, http.StatusUnprocessableEntity, "projects_unsupported", err.Error())
	default:
		s.writeTrackerError(w, r, err)
	}
}

func (s *Server) handlePlanCapabilities(w http.ResponseWriter, r *http.Request) {
	syncer, ok := s.planSyncerFor(w, r)
	if !ok {
		return
	}
	t := syncer.Tracker()
	resp := planCapabilitiesResponse{Capabilities: t.Capabilities()}
	if t.Meta.PlanKeyStorage != "" {
		storage := string(t.Meta.PlanKeyStorage)
		resp.PlanKeyStorage = &storage
	}
	writeJSON(w, s.logger, http.StatusOK, resp)
}

func (s *Server) handlePlanSync(w http.ResponseWriter, r *http.Request) {
	syncer, ok := s.planSyncerFor(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPlanBodyBytes+1))
	if err != nil || len(body) > maxPlanBodyBytes {
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_request", "the request body is unreadable or too large")
		return
	}
	var req planSyncRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_request", "the body is not a plan sync request: "+err.Error())
		return
	}
	if len(req.Graph) == 0 {
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_graph", "the request carries no graph")
		return
	}
	g, err := plan.Decode(req.Graph)
	if err != nil {
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_graph", err.Error())
		return
	}

	if !req.Apply {
		p, id, err := syncer.Preview(r.Context(), g)
		if err != nil {
			s.writeTrackerError(w, r, err)
			return
		}
		writeJSON(w, s.logger, http.StatusOK, planPreviewResponse{PreviewID: id, Preview: p, Capabilities: syncer.Tracker().Capabilities()})
		return
	}

	if req.PreviewID == "" {
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_request", "an apply needs the preview_id of the preview it applies")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(planApplyWriteTimeout))
	result, err := syncer.Apply(r.Context(), g, req.PreviewID)
	switch {
	case err == nil:
		writeJSON(w, s.logger, http.StatusOK, planApplyResponse{Result: result})
	case errors.Is(err, plan.ErrApplyInProgress):
		writeErrorJSON(w, s.logger, http.StatusConflict, "apply_in_progress", err.Error())
	case isConflict(err):
		writeErrorJSON(w, s.logger, http.StatusConflict, "stale_preview", err.Error())
	case result.Failed != nil:
		status, code := trackerErrorStatus(err)
		writeJSON(w, s.logger, status, planApplyResponse{Result: result, Error: &errorDetail{Code: code, Message: err.Error()}})
	default:
		s.writeTrackerError(w, r, err)
	}
	s.logger.Info("plan sync applied",
		slog.Int("created", len(result.Created)), slog.Int("updated", len(result.Updated)),
		slog.Int("relations_added", len(result.RelationsAdded)), slog.Bool("failed", err != nil))
}

func isConflict(err error) bool {
	_, ok := errors.AsType[*plan.ConflictError](err)
	return ok
}

// trackerErrorStatus maps an error to an HTTP status and an error code
// that carries the tracker error kind when there is one.
func trackerErrorStatus(err error) (int, string) {
	te, ok := errors.AsType[*domain.TrackerError](err)
	if !ok {
		return http.StatusBadGateway, "tracker_error"
	}
	switch te.Kind {
	case domain.ErrTrackerUnsupportedOperation:
		return http.StatusUnprocessableEntity, string(te.Kind)
	case domain.ErrTrackerNotFound, domain.ErrTrackerPayload:
		return http.StatusUnprocessableEntity, string(te.Kind)
	default:
		return http.StatusBadGateway, string(te.Kind)
	}
}

func (s *Server) writeTrackerError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := trackerErrorStatus(err)
	s.logger.Warn("plan request failed", slog.String("path", r.URL.Path), slog.Any("error", err))
	writeErrorJSON(w, s.logger, status, code, err.Error())
}

func (s *Server) handlePlanGraph(w http.ResponseWriter, r *http.Request) {
	syncer, ok := s.planSyncerFor(w, r)
	if !ok {
		return
	}
	includeUnkeyed := r.URL.Query().Get("include_unkeyed") == "true"
	t := syncer.Tracker()

	issues := []domain.PlanIssue{}
	if finder, ok := t.Adapter.(domain.PlanKeyFinder); ok {
		got, err := finder.ListPlanIssues(r.Context(), includeUnkeyed)
		if err != nil {
			s.writeTrackerError(w, r, err)
			return
		}
		issues = got
	}

	project := r.URL.Query().Get("project")
	dispatched := s.planProjects.IsDefault(project)
	if project == "" {
		project = s.planProjects.Default()
	}
	var snap orchestrator.RuntimeSnapshotResult
	if s.snapshotFn != nil && dispatched {
		if got, err := s.snapshotFn(); err == nil {
			snap = got
		}
	}
	runtime := runtimeByIssue(snap)

	var terminal []string
	if s.terminalStatesFn != nil {
		terminal = s.terminalStatesFn()
	}

	resp := planGraphResponse{
		Project:      project,
		Dispatched:   dispatched,
		GeneratedAt:  time.Now().UTC(),
		Capabilities: t.Capabilities(),
		Nodes:        make([]planGraphNode, 0, len(issues)),
		Edges:        []planGraphEdge{},
	}
	listed := make(map[string]bool, len(issues))
	for _, i := range issues {
		listed[i.ID] = true
	}
	for _, i := range issues {
		n := planGraphNode{
			ID: i.ID, Identifier: i.Identifier, Title: i.Title, State: i.State,
			Terminal: slices.ContainsFunc(terminal, func(s string) bool { return strings.EqualFold(s, i.State) }),
			PlanKey:  optional(i.PlanKey), Kind: optional(string(i.Kind)), URL: optional(i.URL), ParentID: optional(i.ParentID),
			Runtime: runtime[i.ID],
		}
		resp.Nodes = append(resp.Nodes, n)
		for _, b := range i.BlockedBy {
			if listed[b] {
				resp.Edges = append(resp.Edges, planGraphEdge{BlockerID: b, BlockedID: i.ID})
			}
		}
	}
	writeJSON(w, s.logger, http.StatusOK, resp)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// runtimeByIssue maps issue ids to their runtime state: running wins over
// retrying, which wins over budget held.
func runtimeByIssue(snap orchestrator.RuntimeSnapshotResult) map[string]*planRuntime {
	out := map[string]*planRuntime{}
	for _, b := range snap.BudgetExhausted {
		out[b.IssueID] = &planRuntime{Status: "budget_held"}
	}
	for _, r := range snap.Retrying {
		attempt := r.Attempt
		out[r.IssueID] = &planRuntime{Status: "retrying", Attempt: &attempt}
	}
	for _, r := range snap.Running {
		turns, msg := r.TurnCount, r.LastAgentMessage
		out[r.IssueID] = &planRuntime{Status: "running", TurnCount: &turns, LastMessage: &msg}
	}
	return out
}

// planProjectRequest is the PUT /api/v1/plan/projects/{key} body.
type planProjectRequest struct {
	Name string `json:"name"`
}

// planProjectResponse is GET and PUT /api/v1/plan/projects/{key}.
type planProjectResponse struct {
	Key        string   `json:"key"`
	Name       *string  `json:"name"`
	URL        *string  `json:"url"`
	Exists     bool     `json:"exists"`
	Created    bool     `json:"created"`
	Ready      bool     `json:"ready"`
	Dispatched bool     `json:"dispatched"`
	Problems   []string `json:"problems"`
}

// routePlanProject serves GET (inspect) and PUT (create when missing,
// then inspect) of one tracker project.
func (s *Server) routePlanProject(w http.ResponseWriter, r *http.Request) {
	var handler http.HandlerFunc
	switch r.Method {
	case http.MethodGet:
		handler = s.handlePlanProjectGet
	case http.MethodPut:
		handler = s.handlePlanProjectPut
	default:
		s.methodNotAllowed(w, r, http.MethodGet, http.MethodPut)
		return
	}
	s.requireControl(scopeOperator, handler)(w, r)
}

func (s *Server) handlePlanProjectGet(w http.ResponseWriter, r *http.Request) {
	if s.planProjects == nil {
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable, "plan_unavailable", "plan sync is not configured")
		return
	}
	got, err := s.planProjects.Inspect(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writePlanProjectError(w, r, err)
		return
	}
	writeJSON(w, s.logger, http.StatusOK, s.planProjectBody(got))
}

func (s *Server) handlePlanProjectPut(w http.ResponseWriter, r *http.Request) {
	if s.planProjects == nil {
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable, "plan_unavailable", "plan sync is not configured")
		return
	}
	var req planProjectRequest
	if err := decodeControlBody(r, &req); err != nil {
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_request", "the body is not a project request: "+err.Error())
		return
	}
	got, err := s.planProjects.Ensure(r.Context(), r.PathValue("key"), req.Name)
	if err != nil {
		s.writePlanProjectError(w, r, err)
		return
	}
	if got.Created {
		s.logger.Info("plan project created", slog.String("project", got.Key))
	}
	writeJSON(w, s.logger, http.StatusOK, s.planProjectBody(got))
}

func (s *Server) planProjectBody(p domain.PlanProject) planProjectResponse {
	problems := p.Problems
	if problems == nil {
		problems = []string{}
	}
	return planProjectResponse{
		Key: p.Key, Name: optional(p.Name), URL: optional(p.URL),
		Exists: p.Exists, Created: p.Created, Ready: p.Ready(),
		Dispatched: s.planProjects.IsDefault(p.Key), Problems: problems,
	}
}
