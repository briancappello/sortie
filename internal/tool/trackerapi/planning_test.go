package trackerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// writableAdapter is an in-memory tracker with every write capability.
// Issues in project BILL have identifiers BILL-n; OTHER-1 is outside.
type writableAdapter struct {
	mockTrackerAdapter
	issues map[string]*domain.Issue
	writes []string
}

var (
	_ domain.IssueCreator     = (*writableAdapter)(nil)
	_ domain.IssueUpdater     = (*writableAdapter)(nil)
	_ domain.IssueParenter    = (*writableAdapter)(nil)
	_ domain.DependencyLinker = (*writableAdapter)(nil)
)

func newWritable() *writableAdapter {
	w := &writableAdapter{issues: map[string]*domain.Issue{
		"e1": {ID: "e1", Identifier: "BILL-1", Title: "billing"},
		"x1": {ID: "x1", Identifier: "OTHER-1", Title: "elsewhere"},
	}}
	w.fetchByIDFn = func(_ context.Context, id string) (domain.Issue, error) {
		for _, i := range w.issues {
			if i.ID == id || i.Identifier == id {
				return *i, nil
			}
		}
		return domain.Issue{}, &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: id}
	}
	return w
}

func (w *writableAdapter) CreateIssue(_ context.Context, in domain.NewIssue) (domain.CreatedIssue, error) {
	id := fmt.Sprintf("n%d", len(w.issues))
	issue := &domain.Issue{ID: id, Identifier: fmt.Sprintf("BILL-%d", len(w.issues)+1), Title: in.Title, Description: in.Body}
	if in.ParentID != "" {
		issue.Parent = &domain.ParentRef{ID: in.ParentID}
	}
	w.issues[id] = issue
	w.writes = append(w.writes, "create "+in.PlanKey+" "+string(in.Kind)+" parent="+in.ParentID)
	return domain.CreatedIssue{ID: id, Identifier: issue.Identifier}, nil
}

func (w *writableAdapter) UpdateIssue(_ context.Context, id string, f domain.IssueFields) error {
	if f.Title != nil {
		w.issues[id].Title = *f.Title
	}
	w.writes = append(w.writes, "update "+id)
	return nil
}

func (w *writableAdapter) SetParent(_ context.Context, id, parent string) error {
	w.writes = append(w.writes, "parent "+id+"="+parent)
	return nil
}

func (w *writableAdapter) AddDependency(_ context.Context, a, b string) error {
	w.writes = append(w.writes, "link "+a+">"+b)
	return nil
}

func (w *writableAdapter) RemoveDependency(_ context.Context, a, b string) error {
	w.writes = append(w.writes, "unlink "+a+">"+b)
	return nil
}

func exec(t *testing.T, tool *TrackerAPITool, input string) map[string]any {
	t.Helper()
	raw, err := tool.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func failureCode(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		code, _ := e["kind"].(string)
		return code
	}
	return ""
}

func TestCreateIssueUnderEpic(t *testing.T) {
	t.Parallel()

	w := newWritable()
	tool := New(w, "BILL", WithWriteCapabilities(registry.AllWriteCapabilities()))
	out := exec(t, tool, `{"operation":"create_issue","title":"Tax","body":"- rules","kind":"story","plan_key":"billing/tax","parent_id":"BILL-1"}`)
	if out["success"] != true {
		t.Fatalf("create_issue = %v", out)
	}
	data := out["data"].(map[string]any)
	if data["plan_key"] != "billing/tax" || data["title"] != "Tax" || data["identifier"] == nil {
		t.Errorf("result = %v, want the issue in fetch_issue shape with its plan key", data)
	}
	if !slices.Equal(w.writes, []string{"create billing/tax story parent=e1"}) {
		t.Errorf("writes = %v, want a story created under the epic's id", w.writes)
	}
}

func TestPlanningOperationsAdvertisedOnlyWhenDeclared(t *testing.T) {
	t.Parallel()

	plain := New(newWritable(), "BILL")
	if strings.Contains(plain.Description(), "create_issue") || strings.Contains(string(plain.InputSchema()), "create_issue") {
		t.Error("a tool without write capabilities advertises create_issue")
	}
	out := exec(t, plain, `{"operation":"create_issue","title":"t","kind":"epic","plan_key":"x"}`)
	if failureCode(out) != "unsupported_operation" {
		t.Errorf("create_issue without the capability = %v, want unsupported_operation", out)
	}

	some := New(newWritable(), "BILL", WithWriteCapabilities([]registry.WriteCapability{registry.CapCreateIssue}))
	var schema map[string]any
	if err := json.Unmarshal(some.InputSchema(), &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	enum := schema["properties"].(map[string]any)["operation"].(map[string]any)["enum"].([]any)
	if !slices.Contains(enum, any("create_issue")) || slices.Contains(enum, any("link_dependency")) {
		t.Errorf("operation enum = %v, want create_issue and not link_dependency", enum)
	}
	if !strings.Contains(some.Description(), "create_issue") || strings.Contains(some.Description(), "Planning operations: create_issue, ") {
		t.Errorf("description = %q", some.Description())
	}
	if failureCode(exec(t, some, `{"operation":"link_dependency","blocker_id":"BILL-1","blocked_id":"BILL-1","action":"add"}`)) != "unsupported_operation" {
		t.Error("an undeclared link_dependency is accepted")
	}

	// A declared capability the adapter does not implement stays off.
	notImpl := New(&mockTrackerAdapter{}, "BILL", WithWriteCapabilities(registry.AllWriteCapabilities()))
	if strings.Contains(notImpl.Description(), "create_issue") {
		t.Error("a capability the adapter does not implement is advertised")
	}
}

func TestPlanningOperationsKeepProjectScope(t *testing.T) {
	t.Parallel()

	w := newWritable()
	tool := New(w, "BILL", WithWriteCapabilities(registry.AllWriteCapabilities()))
	for _, input := range []string{
		`{"operation":"link_dependency","blocker_id":"BILL-1","blocked_id":"OTHER-1","action":"add"}`,
		`{"operation":"link_dependency","blocker_id":"OTHER-1","blocked_id":"BILL-1","action":"remove"}`,
		`{"operation":"set_parent","issue_id":"BILL-1","parent_id":"OTHER-1"}`,
		`{"operation":"update_issue","issue_id":"OTHER-1","title":"x"}`,
		`{"operation":"create_issue","title":"t","kind":"story","plan_key":"a/b","parent_id":"OTHER-1"}`,
	} {
		if code := failureCode(exec(t, tool, input)); code != "project_scope_violation" {
			t.Errorf("%s = %q, want project_scope_violation", input, code)
		}
	}
	if len(w.writes) != 0 {
		t.Errorf("writes = %v, want none", w.writes)
	}
}

func TestPlanningOperationsWrite(t *testing.T) {
	t.Parallel()

	w := newWritable()
	w.issues["s1"] = &domain.Issue{ID: "s1", Identifier: "BILL-7", Title: "Story"}
	tool := New(w, "BILL", WithWriteCapabilities(registry.AllWriteCapabilities()))

	out := exec(t, tool, `{"operation":"update_issue","issue_id":"BILL-7","title":"Renamed","kind":"story"}`)
	if out["success"] != true || out["data"].(map[string]any)["title"] != "Renamed" {
		t.Errorf("update_issue = %v", out)
	}
	if out := exec(t, tool, `{"operation":"set_parent","issue_id":"BILL-7","parent_id":null}`); out["success"] != true {
		t.Errorf("set_parent null = %v", out)
	}
	if out := exec(t, tool, `{"operation":"set_parent","issue_id":"BILL-7","parent_id":"BILL-1"}`); out["success"] != true {
		t.Errorf("set_parent = %v", out)
	}
	if out := exec(t, tool, `{"operation":"link_dependency","blocker_id":"BILL-1","blocked_id":"BILL-7","action":"add"}`); out["success"] != true {
		t.Errorf("link_dependency = %v", out)
	}
	want := []string{"update s1", "parent s1=", "parent s1=e1", "link e1>s1"}
	if !slices.Equal(w.writes, want) {
		t.Errorf("writes = %v, want %v", w.writes, want)
	}

	for _, input := range []string{
		`{"operation":"update_issue","issue_id":"BILL-7"}`,
		`{"operation":"set_parent","issue_id":"BILL-7"}`,
		`{"operation":"link_dependency","blocker_id":"BILL-1","blocked_id":"BILL-7","action":"swap"}`,
		`{"operation":"create_issue","title":"t","kind":"bug","plan_key":"x"}`,
		`{"operation":"create_issue","kind":"epic","plan_key":"x"}`,
	} {
		if code := failureCode(exec(t, tool, input)); code != "invalid_input" {
			t.Errorf("%s = %q, want invalid_input", input, code)
		}
	}
}
