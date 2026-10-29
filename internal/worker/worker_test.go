package worker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newM(c *clock, onEnd func(string)) *Matcher {
	return NewMatcher(NewTTLLeaser(time.Minute), c.now, onEnd)
}

func TestCapabilityMatch(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c, nil)
	m.CheckIn(Worker{ID: "a", Capabilities: []string{"linux"}, Free: Resources{1, 100}})
	m.CheckIn(Worker{ID: "b", Capabilities: []string{"linux", "gpu"}, Free: Resources{1, 100}})
	m.Submit(Job{ID: "j", Requires: []string{"gpu"}, DiskMB: 10})
	if _, _, ok, _ := m.Poll("a"); ok {
		t.Fatal("incapable worker got job")
	}
	if _, _, ok, _ := m.Poll("b"); !ok {
		t.Fatal("capable worker got no job")
	}
	if _, _, _, err := m.Poll("zz"); err != ErrUnknownWorker {
		t.Fatal(err)
	}
}

func TestSingleLease(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	l := NewTTLLeaser(time.Minute)
	if _, err := l.Acquire("j", "a", c.t); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire("j", "b", c.t); err != ErrLeaseHeld {
		t.Fatal(err)
	}
	var _ Leaser = l
}

func TestExpiredLeaseEndsAttempt(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	var ended []string
	m := newM(c, func(id string) { ended = append(ended, id) })
	m.CheckIn(Worker{ID: "a", Free: Resources{1, 100}})
	m.Submit(Job{ID: "j", DiskMB: 10})
	m.Poll("a")
	c.t = c.t.Add(30 * time.Second)
	if _, err := m.Renew("j", "a"); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(2 * time.Minute)
	if got := m.ExpireLeases(); len(got) != 1 || len(ended) != 1 {
		t.Fatalf("expired %v ended %v", got, ended)
	}
	if _, err := m.Renew("j", "a"); err != ErrLeaseLost {
		t.Fatal(err)
	}
	if f := m.Snapshot().Workers[0].Free; f != (Resources{1, 100}) {
		t.Fatalf("free %v", f)
	}
}

func TestSlotsAndDisk(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c, nil)
	m.CheckIn(Worker{ID: "a", Free: Resources{1, 100}})
	m.Submit(Job{ID: "j", DiskMB: 40})
	m.Submit(Job{ID: "k", DiskMB: 40})
	m.Poll("a")
	if _, _, ok, _ := m.Poll("a"); ok {
		t.Fatal("no run slot left")
	}
	if err := m.Stop("j", "a"); err != nil {
		t.Fatal(err)
	}
	if f := m.Snapshot().Workers[0].Free; f != (Resources{1, 60}) {
		t.Fatalf("stopped machine must hold disk only: %v", f)
	}
	m.Remove("j", "a")
	if f := m.Snapshot().Workers[0].Free; f != (Resources{1, 100}) {
		t.Fatalf("free %v", f)
	}
}

func TestAPI(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c, nil)
	m.Submit(Job{ID: "j"})
	h := Handler(m)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	if r := do("POST", "/api/v1/workers/w/poll", ""); r.Code != http.StatusNotFound {
		t.Fatal(r.Code)
	}
	if r := do("POST", "/api/v1/workers/checkin", `{"id":"w","free":{"run_slots":1}}`); r.Code != 204 {
		t.Fatal(r.Code)
	}
	if r := do("POST", "/api/v1/workers/w/poll", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if r := do("POST", "/api/v1/workers/w/jobs/j/renew", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	r := do("GET", "/api/v1/workers", "")
	if !strings.Contains(r.Body.String(), `"job_id":"j"`) {
		t.Fatal(r.Body.String())
	}
}
