package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newM(c *clock) *Matcher {
	return NewMatcher(NewTTLLeaser(time.Minute), c.now, nil)
}

func decl(caps []string, r Resources) Declaration {
	return Declaration{Protocol: Protocol, Capabilities: caps, Resources: r}
}

func big() Resources { return Resources{CPUs: 8, MemoryMiB: 8192, DiskMiB: 100, RunSlots: 2} }

func outcome(t *testing.T, ch <-chan Outcome) Outcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	default:
		t.Fatal("job has not ended")
		return Outcome{}
	}
}

// A job goes only to a worker that can run it.
func TestCapabilityMatch(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	m.CheckIn("a", decl([]string{"linux"}, big()))
	m.CheckIn("b", decl([]string{"linux", "gpu"}, big()))
	m.Submit(Job{ID: "j", Requires: []string{"gpu"}, Needs: Resources{DiskMiB: 10}})
	if _, ok, _ := m.Poll("a"); ok {
		t.Fatal("incapable worker got job")
	}
	if _, ok, _ := m.Poll("b"); !ok {
		t.Fatal("capable worker got no job")
	}
	if _, _, err := m.Poll("zz"); !errors.Is(err, ErrUnknownWorker) {
		t.Fatal(err)
	}
}

func TestResourcesChecked(t *testing.T) {
	for name, needs := range map[string]Resources{
		"cpu":    {CPUs: 9},
		"memory": {MemoryMiB: 9000},
		"disk":   {DiskMiB: 101},
	} {
		t.Run(name, func(t *testing.T) {
			c := &clock{time.Unix(0, 0)}
			m := newM(c)
			m.CheckIn("a", decl(nil, big()))
			m.Submit(Job{ID: "j", Needs: needs})
			if _, ok, _ := m.Poll("a"); ok {
				t.Fatal("worker without room got job")
			}
			q := m.Snapshot().Queue
			if len(q) != 1 || len(q[0].Waiting) != 1 || !strings.Contains(q[0].Waiting[0], "worker a:") {
				t.Fatalf("waiting %+v", q)
			}
		})
	}
}

func TestWaitingReasons(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	m.Submit(Job{ID: "j", Requires: []string{"gpu"}})
	if w := m.Snapshot().Queue[0].Waiting; !reflect.DeepEqual(w, []string{"no worker has checked in"}) {
		t.Fatal(w)
	}
	m.CheckIn("a", decl(nil, Resources{CPUs: 1, MemoryMiB: 1, DiskMiB: 1}))
	w := m.Snapshot().Queue[0].Waiting
	if len(w) != 1 || !strings.Contains(w[0], "lacks capability gpu") || !strings.Contains(w[0], "no free run slot") {
		t.Fatal(w)
	}
}

func TestOrderByPriorityThenAge(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	one, two := 1, 2
	m.Submit(Job{ID: "none-old"})
	c.t = c.t.Add(time.Second)
	m.Submit(Job{ID: "two", Priority: &two})
	c.t = c.t.Add(time.Second)
	m.Submit(Job{ID: "one-new", Priority: &one})
	m.Submit(Job{ID: "none-new"})
	m.Submit(Job{ID: "one-newer", Priority: &one, QueuedAt: c.t.Add(time.Second)})
	m.CheckIn("a", decl(nil, Resources{RunSlots: 5}))
	var got []string
	for {
		o, ok, _ := m.Poll("a")
		if !ok {
			break
		}
		got = append(got, o.Job.ID)
	}
	want := []string{"one-new", "one-newer", "two", "none-old", "none-new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v want %v", got, want)
	}
}

func TestSingleLease(t *testing.T) {
	l := NewTTLLeaser(time.Minute)
	now := time.Unix(0, 0)
	if _, err := l.Acquire("j", "a", now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire("j", "b", now); !errors.Is(err, ErrLeaseHeld) {
		t.Fatal(err)
	}
	var _ Leaser = l
}

// An expired lease ends the attempt.
func TestExpiredLeaseEndsAttempt(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	m.CheckIn("a", decl(nil, big()))
	ended := m.Submit(Job{ID: "j", Attempt: 2, Needs: Resources{CPUs: 2, DiskMiB: 10}})
	if _, _, err := m.Poll("a"); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(30 * time.Second)
	if _, err := m.Heartbeat("a", "j", 2); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(2 * time.Minute)
	if got := m.ExpireLeases(); !reflect.DeepEqual(got, []string{"j"}) {
		t.Fatalf("expired %v", got)
	}
	if o := outcome(t, ended); !o.Expired || o.Succeeded {
		t.Fatalf("outcome %+v", o)
	}
	if _, err := m.Heartbeat("a", "j", 2); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	if err := m.Report("a", "j", 2, Outcome{Succeeded: true}); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	if f := m.Snapshot().Workers[0].Free; f != big() {
		t.Fatalf("free %v", f)
	}
}

// Running machines use run slots; stopped ones only disk.
func TestSlotsAndDisk(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	m.CheckIn("a", decl(nil, Resources{CPUs: 4, MemoryMiB: 100, DiskMiB: 100, RunSlots: 1}))
	m.Submit(Job{ID: "j", Needs: Resources{CPUs: 2, MemoryMiB: 50, DiskMiB: 40}})
	m.Submit(Job{ID: "k", Needs: Resources{DiskMiB: 40}})
	if _, _, err := m.Poll("a"); err != nil {
		t.Fatal(err)
	}
	if f := m.Snapshot().Workers[0].Free; f != (Resources{2, 50, 60, 0}) {
		t.Fatalf("running machine: %v", f)
	}
	if _, ok, _ := m.Poll("a"); ok {
		t.Fatal("no run slot left")
	}
	if err := m.Report("a", "j", 0, Outcome{Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	if f := m.Snapshot().Workers[0].Free; f != (Resources{4, 100, 60, 1}) {
		t.Fatalf("stopped machine must hold disk only: %v", f)
	}
	m.Remove("j")
	if f := m.Snapshot().Workers[0].Free; f != (Resources{4, 100, 100, 1}) {
		t.Fatalf("free %v", f)
	}
}

func TestCancelWithdrawsJob(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	m.CheckIn("a", decl(nil, big()))
	ended := m.Submit(Job{ID: "j"})
	if _, _, err := m.Poll("a"); err != nil {
		t.Fatal(err)
	}
	m.Cancel("j")
	if o := outcome(t, ended); !o.Cancelled {
		t.Fatalf("%+v", o)
	}
	if _, err := m.Heartbeat("a", "j", 0); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	if s := m.Snapshot(); len(s.Leases) != 0 || s.Workers[0].Free != big() {
		t.Fatalf("%+v", s)
	}
}

type api struct {
	t *testing.T
	h http.Handler
}

func (a api) post(id, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if id != "" {
		req.Header.Set("X-Worker-Id", id)
	}
	a.h.ServeHTTP(rec, req)
	return rec
}

const fullCheckIn = `{"protocol":1,"capabilities":["linux"],"resources":{"cpus":4,"memory_mib":4096,"disk_mib":1000,"run_slots":1}}`

// The worker's identity comes from one header.
func TestIdentityFromHeader(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	a := api{t, Handler(m, "X-Worker-Id")}
	r := a.post("", "/api/v1/worker/checkin", fullCheckIn)
	if r.Code != http.StatusUnauthorized {
		t.Fatal(r.Code)
	}
	req := httptest.NewRequest("POST", "/api/v1/worker/checkin", strings.NewReader(fullCheckIn))
	req.Header.Set(DefaultIdentityHeader, "w")
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a header other than the configured one named a worker: %d", rec.Code)
	}
	if r := a.post("w", "/api/v1/worker/checkin", fullCheckIn); r.Code != http.StatusOK {
		t.Fatal(r.Code, r.Body)
	}
	if r := a.post("w", "/api/v1/workers/x/poll", ""); r.Code != http.StatusNotFound {
		t.Fatalf("a path naming a worker is served: %d", r.Code)
	}
	if ws := m.Snapshot().Workers; len(ws) != 1 || ws[0].ID != "w" {
		t.Fatal(ws)
	}
}

// Check-in.
func TestCheckIn(t *testing.T) {
	c := &clock{time.Unix(100, 0)}
	m := newM(c)
	a := api{t, Handler(m, "X-Worker-Id")}
	r := a.post("w", "/api/v1/worker/checkin", `{"protocol":1,"resources":{"cpus":4,"disk_mib":1}}`)
	if r.Code != http.StatusBadRequest {
		t.Fatal(r.Code)
	}
	var body struct {
		Missing []string `json:"missing"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &body)
	if want := []string{"capabilities", "resources.memory_mib", "resources.run_slots"}; !reflect.DeepEqual(body.Missing, want) {
		t.Fatalf("missing %v want %v", body.Missing, want)
	}
	if r := a.post("w", "/api/v1/worker/checkin", strings.Replace(fullCheckIn, `"protocol":1`, `"protocol":2`, 1)); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "unsupported_protocol") {
		t.Fatal(r.Code, r.Body)
	}
	if len(m.Snapshot().Workers) != 0 {
		t.Fatal("a rejected check-in was recorded")
	}
	if r := a.post("w", "/api/v1/worker/checkin", fullCheckIn); r.Code != http.StatusOK {
		t.Fatal(r.Code)
	}
	w := m.Snapshot().Workers[0]
	if w.Declaration.Resources != (Resources{4, 4096, 1000, 1}) || !reflect.DeepEqual(w.Declaration.Capabilities, []string{"linux"}) || !w.LastCheckIn.Equal(c.t) {
		t.Fatalf("%+v", w)
	}
}

// Poll returns one offer or none; the run descriptor holds no
// credential; heartbeat renews the lease; report ends the attempt.
func TestPollHeartbeatReport(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	toks := &fakeTokens{}
	m := NewMatcher(NewTTLLeaser(time.Minute), c.now, toks)
	a := api{t, Handler(m, "X-Worker-Id")}
	if r := a.post("w", "/api/v1/worker/poll", ""); r.Code != http.StatusNotFound {
		t.Fatalf("poll before check-in: %d", r.Code)
	}
	a.post("w", "/api/v1/worker/checkin", fullCheckIn)
	if r := a.post("w", "/api/v1/worker/poll", ""); r.Code != http.StatusNoContent {
		t.Fatalf("empty queue: %d", r.Code)
	}
	ended := m.Submit(Job{ID: "j", Identifier: "ONS-1", Attempt: 3, Flows: []string{"git", "tracker"},
		Run: Run{IssueID: "1", Identifier: "ONS-1", Attempt: 3}})
	m.Submit(Job{ID: "k"})
	r := a.post("w", "/api/v1/worker/poll", "")
	if r.Code != http.StatusOK {
		t.Fatal(r.Code)
	}
	var offer Offer
	if err := json.Unmarshal(r.Body.Bytes(), &offer); err != nil {
		t.Fatal(err)
	}
	if offer.Protocol != 1 || offer.Job.ID != "j" || offer.Lease.WorkerID != "w" || offer.Lease.Attempt != 3 || offer.Run.Identifier != "ONS-1" {
		t.Fatalf("%+v", offer)
	}
	if !reflect.DeepEqual(offer.Flows, []Flow{{"git", "tok-j-3-git"}, {"tracker", "tok-j-3-tracker"}}) || !reflect.DeepEqual(toks.minted, []string{"j/3"}) {
		t.Fatalf("flows %+v minted %v", offer.Flows, toks.minted)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(r.Body.Bytes(), &raw)
	if strings.Contains(string(raw["run"]), "tok-") || strings.Contains(string(raw["job"]), "tok-") {
		t.Fatalf("a credential is in the run descriptor: %s", raw["run"])
	}
	if r := a.post("w", "/api/v1/worker/poll", ""); r.Code != http.StatusNoContent {
		t.Fatalf("one offer per free slot: %d", r.Code)
	}

	c.t = c.t.Add(50 * time.Second)
	r = a.post("w", "/api/v1/worker/jobs/j/attempts/3/heartbeat", "")
	var ls LeaseView
	_ = json.Unmarshal(r.Body.Bytes(), &ls)
	if r.Code != http.StatusOK || !ls.Expires.Equal(c.t.Add(time.Minute)) {
		t.Fatal(r.Code, r.Body)
	}
	if r := a.post("w", "/api/v1/worker/jobs/j/attempts/2/heartbeat", ""); r.Code != http.StatusConflict {
		t.Fatalf("another attempt: %d", r.Code)
	}
	if r := a.post("x", "/api/v1/worker/jobs/j/attempts/3/heartbeat", ""); r.Code != http.StatusConflict {
		t.Fatalf("another worker: %d", r.Code)
	}
	if r := a.post("w", "/api/v1/worker/jobs/j/attempts/3/report", `{"outcome":"done"}`); r.Code != http.StatusBadRequest {
		t.Fatal(r.Code)
	}
	if r := a.post("w", "/api/v1/worker/jobs/j/attempts/3/report", `{"outcome":"failed","error":"tests failed"}`); r.Code != http.StatusNoContent {
		t.Fatal(r.Code, r.Body)
	}
	if o := outcome(t, ended); o.Succeeded || o.Error != "tests failed" {
		t.Fatalf("%+v", o)
	}
	if r := a.post("w", "/api/v1/worker/jobs/j/attempts/3/report", `{"outcome":"succeeded"}`); r.Code != http.StatusConflict {
		t.Fatalf("second report: %d", r.Code)
	}
}

type fakeTokens struct {
	minted, voided []string
	err            error
}

func (f *fakeTokens) Mint(j Job) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.minted = append(f.minted, fmt.Sprintf("%s/%d", j.ID, j.Attempt))
	out := map[string]string{}
	for _, fl := range j.Flows {
		out[fl] = fmt.Sprintf("tok-%s-%d-%s", j.ID, j.Attempt, fl)
	}
	return out, nil
}

func (f *fakeTokens) Void(jobID string) { f.voided = append(f.voided, jobID) }

// An attempt's tokens are minted once at the grant and voided on every
// way the attempt ends: report, lease expiry and cancel.
func TestTokensMintedAtGrantAndVoidedAtEnd(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	toks := &fakeTokens{}
	m := NewMatcher(NewTTLLeaser(time.Minute), c.now, toks)
	m.CheckIn("w", decl(nil, Resources{CPUs: 8, MemoryMiB: 8192, DiskMiB: 100, RunSlots: 3}))
	for _, id := range []string{"r", "e", "c"} {
		m.Submit(Job{ID: id, Attempt: 1, Flows: []string{"stdio", "mcp"}})
	}
	for range 3 {
		if _, ok, err := m.Poll("w"); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	if !reflect.DeepEqual(toks.minted, []string{"r/1", "e/1", "c/1"}) || len(toks.voided) != 0 {
		t.Fatalf("minted %v voided %v", toks.minted, toks.voided)
	}
	if err := m.Report("w", "r", 1, Outcome{Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	m.Cancel("c")
	if !reflect.DeepEqual(toks.voided, []string{"r", "c"}) {
		t.Fatalf("voided %v", toks.voided)
	}
	c.t = c.t.Add(2 * time.Minute)
	m.ExpireLeases()
	if !reflect.DeepEqual(toks.voided, []string{"r", "c", "e"}) {
		t.Fatalf("voided %v", toks.voided)
	}
}

// A grant whose tokens cannot be minted is undone: the job stays
// queued and the worker keeps its slot.
func TestFailedMintLeavesJobQueued(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	toks := &fakeTokens{err: errors.New("no entropy")}
	m := NewMatcher(NewTTLLeaser(time.Minute), c.now, toks)
	m.CheckIn("w", decl(nil, big()))
	m.Submit(Job{ID: "j", Flows: []string{"stdio"}})
	if _, ok, err := m.Poll("w"); ok || err == nil {
		t.Fatalf("poll ok=%v err=%v", ok, err)
	}
	s := m.Snapshot()
	if len(s.Queue) != 1 || len(s.Leases) != 0 || s.Workers[0].Free != big() {
		t.Fatalf("%+v", s)
	}
	toks.err = nil
	if _, ok, err := m.Poll("w"); !ok || err != nil {
		t.Fatalf("retry ok=%v err=%v", ok, err)
	}
}

func TestDefaultTokenIsEmpty(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	m.CheckIn("w", decl(nil, big()))
	m.Submit(Job{ID: "j", Flows: []string{"git"}})
	o, _, _ := m.Poll("w")
	if !reflect.DeepEqual(o.Flows, []Flow{{Name: "git"}}) {
		t.Fatal(o.Flows)
	}
}

func TestLateHeartbeatAfterExpiry(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	a := api{t, Handler(m, "X-Worker-Id")}
	a.post("w", "/api/v1/worker/checkin", fullCheckIn)
	m.Submit(Job{ID: "j"})
	a.post("w", "/api/v1/worker/poll", "")
	c.t = c.t.Add(2 * time.Minute)
	m.ExpireLeases()
	for _, p := range []string{"heartbeat", "report"} {
		if r := a.post("w", "/api/v1/worker/jobs/j/attempts/0/"+p, `{"outcome":"succeeded"}`); r.Code != http.StatusConflict {
			t.Fatalf("late %s: %d", p, r.Code)
		}
	}
}

// Workers dial out and are never dialed: a whole run takes only
// requests the worker makes, and a worker declares no address.
func TestWorkersDialOut(t *testing.T) {
	if _, ok := reflect.TypeFor[Declaration]().FieldByName("Address"); ok {
		t.Fatal("declaration carries an address to dial")
	}
	c := &clock{time.Unix(0, 0)}
	m := newM(c)
	srv := httptest.NewServer(Handler(m, "X-Worker-Id"))
	defer srv.Close()
	ended := m.Submit(Job{ID: "j"})
	post := func(path, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Worker-Id", "w")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, step := range []struct {
		path, body string
		code       int
	}{
		{"/api/v1/worker/checkin", fullCheckIn, 200},
		{"/api/v1/worker/poll", "", 200},
		{"/api/v1/worker/jobs/j/attempts/0/heartbeat", "", 200},
		{"/api/v1/worker/jobs/j/attempts/0/report", `{"outcome":"succeeded"}`, 204},
	} {
		if got := post(step.path, step.body); got != step.code {
			t.Fatalf("%s: %d", step.path, got)
		}
	}
	if o := outcome(t, ended); !o.Succeeded {
		t.Fatal(o)
	}
}
