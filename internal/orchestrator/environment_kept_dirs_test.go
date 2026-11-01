package orchestrator

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
)

// keepingEnvironment names each workspace's instance and keeps one host
// directory per workspace, standing in for a kind that mounts a
// per-workspace directory into its VMs.
type keepingEnvironment struct{ namingEnvironment }

var _ environment.KeptDirLister = keepingEnvironment{}

func (keepingEnvironment) KeptDirs(ws environment.Workspace) []string {
	return []string{"/state/recorder/" + ws.Key}
}

func TestRuntimeSnapshot_KeptDirs(t *testing.T) {
	t.Parallel()

	state := NewState(1000, 4, 0, nil, AgentTotals{})
	state.Running["id-run"] = &RunningEntry{
		Identifier: "VM-1", Issue: domain.Issue{ID: "id-run", Identifier: "VM-1"},
		Environment: environment.Instance{Kind: "vmkind", Name: "vm-VM-1"},
	}
	state.Running["id-hold"] = &RunningEntry{
		Identifier: "VM-2", Issue: domain.Issue{ID: "id-hold", Identifier: "VM-2"},
		Environment: environment.Instance{Kind: "vmkind", Name: "vm-VM-2"},
		HeldSince:   time.Now().Add(-time.Minute),
	}
	state.Running["id-host"] = &RunningEntry{Identifier: "HOST-1", Issue: domain.Issue{ID: "id-host", Identifier: "HOST-1"}}
	state.RetryAttempts["id-retry"] = &RetryEntry{IssueID: "id-retry", Identifier: "VM-3", Attempt: 1}
	state.Held = map[string]*HeldEntry{}
	state.Held["id-held"] = &HeldEntry{Identifier: "VM-4", Since: time.Now().UTC(), Reason: SkipSSHHostsFull}
	state.DescribeEnvironment = func(identifier string) environment.Instance {
		if identifier == "HOST-1" {
			return environment.Instance{}
		}
		return environment.Instance{Kind: "vmkind", Name: "vm-" + identifier}
	}
	state.DescribeKeptDirs = func(identifier string) []string {
		return []string{"/state/recorder/" + identifier}
	}

	snap := RuntimeSnapshot(state, time.Now())
	got := map[string]*SnapshotEnvironment{}
	for _, e := range snap.Running {
		got[e.Identifier] = e.Environment
	}
	for _, e := range snap.Retrying {
		got[e.Identifier] = e.Environment
	}
	for _, e := range snap.Held {
		got[e.Identifier] = e.Environment
	}
	for _, id := range []string{"VM-1", "VM-2", "VM-3", "VM-4"} {
		env, ok := got[id]
		if !ok || env == nil || !slices.Equal(env.KeptDirs, []string{"/state/recorder/" + id}) {
			t.Errorf("snapshot environment of %s = %+v (listed %v), want the kept directory /state/recorder/%s", id, env, ok, id)
		}
	}
	if env := got["HOST-1"]; env != nil {
		t.Errorf("snapshot environment of HOST-1 = %+v, want nil for a workspace that runs on the host", env)
	}

	state.DescribeKeptDirs = nil
	for _, e := range RuntimeSnapshot(state, time.Now()).Retrying {
		if e.Environment == nil || e.Environment.KeptDirs != nil {
			t.Errorf("retry %s environment = %+v without DescribeKeptDirs, want the instance without kept directories", e.Identifier, e.Environment)
		}
	}
}

func TestDescribeKeptDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := config.ServiceConfig{Workspace: config.WorkspaceConfig{Root: root}, Environment: config.EnvironmentConfig{Kind: "vmkind"}}
	tests := []struct {
		name string
		env  environment.Environment
		want []string
	}{
		{name: "kept directories", env: keepingEnvironment{namingEnvironment{&environmenttest.Recorder{KindName: "vmkind"}}},
			want: []string{"/state/recorder/ENG_42"}},
		{name: "no kept directories", env: namingEnvironment{&environmenttest.Recorder{KindName: "vmkind"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolver := newEnvironmentResolver(filepath.Join(t.TempDir(), "sortie.db"), "", slog.New(slog.DiscardHandler))
			resolver.cfg, resolver.env = cfg.Environment, tt.env
			o := &Orchestrator{environments: resolver}
			if got := o.describeKeptDirs(cfg)("ENG/42"); !slices.Equal(got, tt.want) {
				t.Errorf("describeKeptDirs()(ENG/42) = %q, want %q", got, tt.want)
			}
		})
	}
}
