package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
	"github.com/sortie-ai/sortie/internal/worker"
)

type remoteClock struct{ t time.Time }

func (c *remoteClock) now() time.Time { return c.t }

func remoteCfg() config.ServiceConfig {
	return config.ServiceConfig{Environment: config.EnvironmentConfig{
		Kind: config.EnvironmentKindRemote,
		Remote: config.RemoteConfig{
			IdentityHeader: "X-W", Requires: []string{"linux"}, CPUs: 2, MemoryMiB: 512, DiskMiB: 10,
			Flows: []string{"git"}, LeaseTTLMS: 60000,
		},
	}}
}

// runRemote starts RunRemoteAttempt and returns the channel its result
// arrives on, once the job is queued.
func runRemote(t *testing.T, ctx context.Context, m *worker.Matcher, issue domain.Issue, attempt *int) <-chan WorkerResult {
	t.Helper()
	done := make(chan WorkerResult, 1)
	go RunRemoteAttempt(withDispatchID(ctx, "D1"), m, remoteCfg(), issue, attempt, "claude-code", func(_ string, r WorkerResult) { done <- r })
	waittest.Until(t, func() error {
		if len(m.Snapshot().Queue) == 0 {
			return errors.New("job not queued")
		}
		return nil
	})
	return done
}

// The remote kind hands runs to the matcher.
func TestRemoteKindHandsRunToMatcher(t *testing.T) {
	c := &remoteClock{time.Unix(0, 0)}
	m := worker.NewMatcher(worker.NewTTLLeaser(time.Minute), c.now, nil)
	prio := 2
	issue := domain.Issue{ID: "1", Identifier: "ONS-7", Title: "t", Priority: &prio}
	done := runRemote(t, context.Background(), m, issue, new(3))
	q := m.Snapshot().Queue[0].Job
	want := worker.Job{ID: "D1", Identifier: "ONS-7", Attempt: 3, Priority: &prio, Requires: []string{"linux"},
		Needs: worker.Resources{CPUs: 2, MemoryMiB: 512, DiskMiB: 10}, Flows: []string{"git"}}
	q.QueuedAt, q.Run = time.Time{}, worker.Run{}
	if !reflect.DeepEqual(q, want) {
		t.Fatalf("job %+v\nwant %+v", q, want)
	}
	m.CheckIn("w", worker.Declaration{Protocol: 1, Capabilities: []string{"linux"}, Resources: worker.Resources{CPUs: 2, MemoryMiB: 512, DiskMiB: 10, RunSlots: 1}})
	offer, ok, _ := m.Poll("w")
	if !ok || offer.Run.Identifier != "ONS-7" || offer.Run.WorkspaceKey != "ONS-7" || offer.Run.AgentKind != "claude-code" {
		t.Fatalf("%+v", offer)
	}
	if err := m.Report("w", "D1", 3, worker.Outcome{Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	r := waittest.Receive(t, done, "remote attempt result")
	if r.ExitKind != WorkerExitNormal || r.Identifier != "ONS-7" || *r.Attempt != 3 || r.WorkspacePath != "" {
		t.Fatalf("%+v", r)
	}
}

// An expired lease ends the attempt with an error exit, which the exit
// handler retries as any failed attempt.
func TestRemoteLeaseExpiryEndsAttempt(t *testing.T) {
	c := &remoteClock{time.Unix(0, 0)}
	m := worker.NewMatcher(worker.NewTTLLeaser(time.Minute), c.now, nil)
	done := runRemote(t, context.Background(), m, domain.Issue{ID: "1", Identifier: "ONS-7"}, nil)
	m.CheckIn("w", worker.Declaration{Protocol: 1, Capabilities: []string{"linux"}, Resources: worker.Resources{CPUs: 4, MemoryMiB: 1024, DiskMiB: 100, RunSlots: 1}})
	if _, ok, _ := m.Poll("w"); !ok {
		t.Fatal("no offer")
	}
	c.t = c.t.Add(2 * time.Minute)
	m.ExpireLeases()
	r := waittest.Receive(t, done, "remote attempt result")
	if r.ExitKind != WorkerExitError || r.Error == nil {
		t.Fatalf("%+v", r)
	}
	if _, err := m.Heartbeat("w", "D1", 0); err == nil {
		t.Fatal("late heartbeat accepted")
	}
}

func TestRemoteAttemptCancelled(t *testing.T) {
	c := &remoteClock{time.Unix(0, 0)}
	m := worker.NewMatcher(worker.NewTTLLeaser(time.Minute), c.now, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := runRemote(t, ctx, m, domain.Issue{ID: "1", Identifier: "ONS-7"}, nil)
	cancel()
	if r := waittest.Receive(t, done, "remote attempt result"); r.ExitKind != WorkerExitCancelled {
		t.Fatalf("%+v", r)
	}
	if q := m.Snapshot().Queue; len(q) != 0 {
		t.Fatalf("withdrawn job still queued: %+v", q)
	}
}
