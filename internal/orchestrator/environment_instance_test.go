package orchestrator

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
)

// namingEnvironment is a recording environment that names each
// workspace's instance after its key, standing in for a kind that runs
// workspaces in VMs.
type namingEnvironment struct{ *environmenttest.Recorder }

var _ environment.Namer = namingEnvironment{}

func (namingEnvironment) InstanceName(ws environment.Workspace) string { return "vm-" + ws.Key }

func TestRuntimeSnapshot_Environment(t *testing.T) {
	t.Parallel()

	state := NewState(1000, 4, 0, nil, AgentTotals{})
	state.Running["id-vm"] = &RunningEntry{
		Identifier: "VM-1", Issue: domain.Issue{ID: "id-vm", Identifier: "VM-1"},
		Environment: environment.Instance{Kind: "vmkind", Name: "vm-VM-1", ImageDigest: "abc"},
	}
	state.Running["id-host"] = &RunningEntry{Identifier: "HOST-1", Issue: domain.Issue{ID: "id-host", Identifier: "HOST-1"}}
	state.RetryAttempts["id-vm-retry"] = &RetryEntry{IssueID: "id-vm-retry", Identifier: "VM-2", Attempt: 1}
	state.RetryAttempts["id-host-retry"] = &RetryEntry{IssueID: "id-host-retry", Identifier: "HOST-2", Attempt: 1}
	state.DescribeEnvironment = func(identifier string) environment.Instance {
		if identifier == "VM-2" {
			return environment.Instance{Kind: "vmkind", Name: "vm-VM-2"}
		}
		return environment.Instance{}
	}

	snap := RuntimeSnapshot(state, time.Now())
	want := map[string]*SnapshotEnvironment{
		"VM-1": {Kind: "vmkind", Name: "vm-VM-1"}, "HOST-1": nil,
		"VM-2": {Kind: "vmkind", Name: "vm-VM-2"}, "HOST-2": nil,
	}
	got := map[string]*SnapshotEnvironment{}
	for _, e := range snap.Running {
		got[e.Identifier] = e.Environment
	}
	for _, e := range snap.Retrying {
		got[e.Identifier] = e.Environment
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok || (w == nil) != (g == nil) || (w != nil && *w != *g) {
			t.Errorf("snapshot environment of %s = %+v, want %+v", id, g, w)
		}
	}

	state.DescribeEnvironment = nil
	for _, e := range RuntimeSnapshot(state, time.Now()).Retrying {
		if e.Environment != nil {
			t.Errorf("retry %s environment = %+v without DescribeEnvironment, want nil", e.Identifier, e.Environment)
		}
	}
}

func TestDispatchIssue_RecordsTheEnvironmentInstance(t *testing.T) {
	t.Parallel()

	state := NewState(1000, 4, 0, nil, AgentTotals{})
	state.DescribeEnvironment = func(identifier string) environment.Instance {
		return environment.Instance{Kind: "vmkind", Name: "vm-" + identifier}
	}
	DispatchIssue(context.Background(), state, domain.Issue{ID: "id-1", Identifier: "VM-1"}, nil, "", func(context.Context, domain.Issue, *int) {})
	state.WorkerWg.Wait()

	if got := state.Running["id-1"].Environment; got != (environment.Instance{Kind: "vmkind", Name: "vm-VM-1"}) {
		t.Errorf("running entry environment = %+v, want the described instance", got)
	}
}

func TestDescribeEnvironment(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := config.ServiceConfig{Workspace: config.WorkspaceConfig{Root: root}, Environment: config.EnvironmentConfig{Kind: "vmkind"}}
	tests := []struct {
		name string
		env  environment.Environment
		want environment.Instance
	}{
		{name: "named instances", env: namingEnvironment{&environmenttest.Recorder{KindName: "vmkind"}},
			want: environment.Instance{Kind: "vmkind", Name: "vm-ENG-42"}},
		{name: "no named instances", env: &environmenttest.Recorder{KindName: "vmkind"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolver := newEnvironmentResolver(filepath.Join(t.TempDir(), "sortie.db"), "", slog.New(slog.DiscardHandler))
			resolver.cfg, resolver.env = cfg.Environment, tt.env
			o := &Orchestrator{environments: resolver}
			if got := o.describeEnvironment(cfg)("ENG-42"); got != tt.want {
				t.Errorf("describeEnvironment()(ENG-42) = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHandleWorkerExit_RecordsTheEnvironmentImage(t *testing.T) {
	t.Parallel()

	for name, instance := range map[string]environment.Instance{
		"VM":   {Kind: "vmkind", Name: "vm-ENVIMG-1", ImageDigest: "0123abcd"},
		"host": {},
	} {
		store := &mockExitStore{}
		state := exitState(t, "ENVIMG-1", nil)
		HandleWorkerExit(state, WorkerResult{
			IssueID: "ENVIMG-1", Identifier: "ENVIMG-1-ident", ExitKind: WorkerExitCancelled, AgentAdapter: "mock",
			EnvironmentInstance: instance,
		}, defaultExitParams(t, store))
		if len(store.runHistories) != 1 || store.runHistories[0].EnvironmentImageSHA256 != instance.ImageDigest {
			t.Errorf("%s: run histories = %+v, want one recording image %q", name, store.runHistories, instance.ImageDigest)
		}
		CancelRetry(state, "ENVIMG-1")
	}
}
