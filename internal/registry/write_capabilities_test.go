package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// readOnlyAdapter implements the core contract and no write capability.
type readOnlyAdapter struct{ domain.TrackerAdapter }

// keyedAdapter adds plan-key lookup and issue creation, and nothing else.
type keyedAdapter struct{ domain.TrackerAdapter }

func (*keyedAdapter) FindIssueByPlanKey(context.Context, string) (domain.PlanIssue, error) {
	return domain.PlanIssue{}, nil
}

func (*keyedAdapter) ListPlanIssues(context.Context, bool) ([]domain.PlanIssue, error) {
	return nil, nil
}

func (*keyedAdapter) CreateIssue(context.Context, domain.NewIssue) (domain.CreatedIssue, error) {
	return domain.CreatedIssue{}, nil
}

// fullAdapter implements every write capability.
type fullAdapter struct{ keyedAdapter }

func (*fullAdapter) UpdateIssue(context.Context, string, domain.IssueFields) error  { return nil }
func (*fullAdapter) SetParent(context.Context, string, string) error                { return nil }
func (*fullAdapter) AddDependency(context.Context, string, string) error            { return nil }
func (*fullAdapter) RemoveDependency(context.Context, string, string) error         { return nil }
func (*fullAdapter) SetApproval(context.Context, string, domain.PlanApproval) error { return nil }

// creatorWithoutFinder creates issues but cannot find them by plan key.
type creatorWithoutFinder struct{ domain.TrackerAdapter }

func (*creatorWithoutFinder) CreateIssue(context.Context, domain.NewIssue) (domain.CreatedIssue, error) {
	return domain.CreatedIssue{}, nil
}

func nopTracker(map[string]any) (domain.TrackerAdapter, error) { return nil, nil }

func registerPanic(t *testing.T, meta TrackerMeta) (msg string) {
	t.Helper()
	r := newTrackerRegistry()
	defer func() {
		if v := recover(); v != nil {
			msg, _ = v.(string)
		}
	}()
	r.RegisterWithMeta("x", nopTracker, meta)
	return ""
}

func TestTrackerRegistry_RejectsUndeclaredImplementation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		meta    TrackerMeta
		wantMsg string
	}{
		{
			name: "declared but not implemented",
			meta: TrackerMeta{
				WriteCapabilities: []WriteCapability{CapCreateIssue, CapLinkDependency},
				PlanKeyStorage:    PlanKeyCustomField,
				Implementation:    (*keyedAdapter)(nil),
			},
			wantMsg: `declares write capability "link_dependency" and does not implement it`,
		},
		{
			name: "no implementation to check",
			meta: TrackerMeta{
				WriteCapabilities: []WriteCapability{CapCreateIssue},
				PlanKeyStorage:    PlanKeyCustomField,
			},
			wantMsg: "without an Implementation",
		},
		{
			name: "no plan key storage",
			meta: TrackerMeta{
				WriteCapabilities: []WriteCapability{CapCreateIssue},
				Implementation:    (*keyedAdapter)(nil),
			},
			wantMsg: "without a PlanKeyStorage",
		},
		{
			name: "no plan key finder",
			meta: TrackerMeta{
				WriteCapabilities: []WriteCapability{CapCreateIssue},
				PlanKeyStorage:    PlanKeyBodyMarker,
				Implementation:    (*creatorWithoutFinder)(nil),
			},
			wantMsg: "does not implement domain.PlanKeyFinder",
		},
		{
			name: "unknown capability",
			meta: TrackerMeta{
				WriteCapabilities: []WriteCapability{"delete_issue"},
				PlanKeyStorage:    PlanKeyCustomField,
				Implementation:    (*fullAdapter)(nil),
			},
			wantMsg: `unknown write capability "delete_issue"`,
		},
		{
			name: "duplicate capability",
			meta: TrackerMeta{
				WriteCapabilities: []WriteCapability{CapSetParent, CapSetParent},
				PlanKeyStorage:    PlanKeyCustomField,
				Implementation:    (*fullAdapter)(nil),
			},
			wantMsg: "declared twice",
		},
		{
			name: "unknown plan key storage",
			meta: TrackerMeta{
				WriteCapabilities: []WriteCapability{CapCreateIssue},
				PlanKeyStorage:    "label",
				Implementation:    (*fullAdapter)(nil),
			},
			wantMsg: `unknown plan key storage "label"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg := registerPanic(t, tt.meta)
			if msg == "" {
				t.Fatal("RegisterWithMeta did not panic")
			}
			if !strings.Contains(msg, tt.wantMsg) || !strings.Contains(msg, `tracker kind "x"`) {
				t.Errorf("panic = %q, want it to contain %q and name the kind", msg, tt.wantMsg)
			}
		})
	}
}

func TestTrackerRegistry_AcceptsImplementedCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta TrackerMeta
	}{
		{"no capabilities", TrackerMeta{}},
		{"read-only implementation without capabilities", TrackerMeta{Implementation: (*readOnlyAdapter)(nil)}},
		{"a subset", TrackerMeta{
			WriteCapabilities: []WriteCapability{CapCreateIssue},
			PlanKeyStorage:    PlanKeyCustomField,
			Implementation:    (*keyedAdapter)(nil),
		}},
		{"all four", TrackerMeta{
			WriteCapabilities: AllWriteCapabilities(),
			PlanKeyStorage:    PlanKeyBodyMarker,
			Implementation:    (*fullAdapter)(nil),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if msg := registerPanic(t, tt.meta); msg != "" {
				t.Fatalf("RegisterWithMeta panicked: %s", msg)
			}
		})
	}
}

func TestTrackerMeta_HasWriteCapability(t *testing.T) {
	t.Parallel()

	m := TrackerMeta{WriteCapabilities: []WriteCapability{CapSetParent}}
	if !m.HasWriteCapability(CapSetParent) {
		t.Error("HasWriteCapability(set_parent) = false, want true")
	}
	if m.HasWriteCapability(CapLinkDependency) {
		t.Error("HasWriteCapability(link_dependency) = true, want false")
	}
	if (TrackerMeta{}).HasWriteCapability(CapCreateIssue) {
		t.Error("zero TrackerMeta reports a write capability")
	}
}

func TestPackageTrackersRegistryChecksMeta(t *testing.T) {
	t.Parallel()

	if Trackers.checkMeta == nil {
		t.Fatal("registry.Trackers does not check declared metadata at registration")
	}
}
