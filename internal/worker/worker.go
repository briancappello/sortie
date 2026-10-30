// Package worker matches jobs to workers that dial out to Sortie. A
// worker checks in with its capabilities and free resources, polls for
// a job, and holds a lease on it that it renews by heartbeat until it
// reports the attempt's outcome. The rule that decides who holds a
// lease sits behind [Leaser].
package worker

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Protocol is the worker API protocol version this package serves.
const Protocol = 1

// Errors reported by the matcher and the lease rule.
var (
	ErrUnknownWorker = errors.New("worker: unknown worker")
	ErrUnknownLease  = errors.New("worker: unknown lease")
	ErrLeaseHeld     = errors.New("worker: job already leased")
	ErrLeaseLost     = errors.New("worker: lease expired or held by another worker")
)

// Resources are amounts of a worker's capacity: what it has free, or
// what a job needs.
type Resources struct {
	CPUs      int   `json:"cpus"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskMiB   int64 `json:"disk_mib"`
	// RunSlots counts machines the worker can run at once. A job needs
	// one.
	RunSlots int `json:"run_slots"`
}

// Declaration is what a worker states at check-in.
type Declaration struct {
	Protocol     int       `json:"protocol"`
	Capabilities []string  `json:"capabilities"`
	Resources    Resources `json:"resources"`
}

// Worker is a checked-in worker.
type Worker struct {
	ID          string      `json:"id"`
	Declaration Declaration `json:"declaration"`
	// Free is what the worker has free now: the declaration less what
	// the jobs it holds use.
	Free        Resources `json:"free"`
	LastCheckIn time.Time `json:"last_check_in"`
}

// Run is the run descriptor a worker receives with an offer: what to
// run, never a credential. A flow's credential travels apart, as
// [Flow.Token].
type Run struct {
	IssueID      string `json:"issue_id"`
	Identifier   string `json:"identifier"`
	Title        string `json:"title,omitempty"`
	URL          string `json:"url,omitempty"`
	Attempt      int    `json:"attempt"`
	AgentKind    string `json:"agent_kind,omitempty"`
	WorkspaceKey string `json:"workspace_key,omitempty"`
}

// Job is one attempt of a run handed to the matcher by the remote
// environment kind.
type Job struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Attempt    int    `json:"attempt"`
	// Priority is the plan priority; lower runs first and nil runs
	// after every set priority.
	Priority *int `json:"priority"`
	// Requires lists capabilities a worker must all have.
	Requires []string `json:"requires"`
	// Needs is what the job's machine takes from a worker, besides a
	// run slot.
	Needs Resources `json:"needs"`
	// Flows names the flows the run reaches; each gets a token in the
	// offer.
	Flows    []string  `json:"flows"`
	Run      Run       `json:"-"`
	QueuedAt time.Time `json:"queued_at"`
}

// Lease is the right of one worker to run one job until Expires.
type Lease struct {
	JobID    string    `json:"job_id"`
	WorkerID string    `json:"worker_id"`
	Expires  time.Time `json:"expires"`
}

// Flow is a flow of an offer with the token that admits the run to it.
type Flow struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// Offer is what a poll hands a worker.
type Offer struct {
	Protocol int       `json:"protocol"`
	Job      Job       `json:"job"`
	Lease    LeaseView `json:"lease"`
	Run      Run       `json:"run"`
	Flows    []Flow    `json:"flows"`
}

// Tokens issues the flow tokens of an attempt; it is the one place they
// come from. The matcher calls Mint once when it grants the attempt's
// lease, for a token per flow of the job, and Void as soon as the
// attempt ends, by report, lease expiry or cancel, after which those
// tokens must be refused. Implementations must be safe for concurrent
// use; the matcher calls them under its lock.
type Tokens interface {
	Mint(job Job) (map[string]string, error)
	Void(jobID string)
}

// NoTokens is the issuer used when job tokens are off: every token is
// empty.
type NoTokens struct{}

// Mint implements [Tokens] with no tokens.
func (NoTokens) Mint(Job) (map[string]string, error) { return nil, nil }

// Void implements [Tokens].
func (NoTokens) Void(string) {}

// Outcome is how a job's attempt ended.
type Outcome struct {
	// Succeeded is the worker's report of success.
	Succeeded bool
	// Error is the worker's failure message, or why Sortie ended it.
	Error string
	// Expired reports that the lease expired before a report.
	Expired bool
	// Cancelled reports that Sortie withdrew the job.
	Cancelled bool
}

// Leaser is the lease rule: who holds a job and until when. Only one
// worker holds a job at a time. Implementations must be safe for
// concurrent use.
type Leaser interface {
	// Acquire grants jobID to workerID, or returns [ErrLeaseHeld] while
	// an unexpired lease on it exists.
	Acquire(jobID, workerID string, now time.Time) (Lease, error)
	// Renew extends the lease, or returns [ErrLeaseLost].
	Renew(jobID, workerID string, now time.Time) (Lease, error)
	// Release drops the lease.
	Release(jobID, workerID string) error
	// Expired removes and returns the leases that ended before now.
	Expired(now time.Time) []Lease
	// Leases returns the current leases ordered by job.
	Leases() []Lease
}

// TTLLeaser is the fixed-duration rule: a lease lasts TTL from its
// grant or last renewal, and a missed renewal ends it.
type TTLLeaser struct {
	mu     sync.Mutex
	ttl    time.Duration
	leases map[string]Lease
}

// NewTTLLeaser returns a leaser whose leases last ttl.
func NewTTLLeaser(ttl time.Duration) *TTLLeaser {
	return &TTLLeaser{ttl: ttl, leases: map[string]Lease{}}
}

// SetTTL changes the duration of leases granted or renewed from now on.
func (l *TTLLeaser) SetTTL(ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ttl = ttl
}

// Acquire implements [Leaser].
func (l *TTLLeaser) Acquire(jobID, workerID string, now time.Time) (Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.leases[jobID]; ok && now.Before(cur.Expires) {
		return Lease{}, ErrLeaseHeld
	}
	ls := Lease{JobID: jobID, WorkerID: workerID, Expires: now.Add(l.ttl)}
	l.leases[jobID] = ls
	return ls, nil
}

// Renew implements [Leaser].
func (l *TTLLeaser) Renew(jobID, workerID string, now time.Time) (Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.leases[jobID]
	if !ok || cur.WorkerID != workerID || !now.Before(cur.Expires) {
		return Lease{}, ErrLeaseLost
	}
	cur.Expires = now.Add(l.ttl)
	l.leases[jobID] = cur
	return cur, nil
}

// Release implements [Leaser].
func (l *TTLLeaser) Release(jobID, workerID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.leases[jobID]
	if !ok || cur.WorkerID != workerID {
		return ErrUnknownLease
	}
	delete(l.leases, jobID)
	return nil
}

// Expired implements [Leaser].
func (l *TTLLeaser) Expired(now time.Time) []Lease {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Lease
	for id, ls := range l.leases {
		if !now.Before(ls.Expires) {
			out = append(out, ls)
			delete(l.leases, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

// Leases implements [Leaser].
func (l *TTLLeaser) Leases() []Lease {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Lease, 0, len(l.leases))
	for _, ls := range l.leases {
		out = append(out, ls)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

// LeaseView is a live lease as the state API shows it.
type LeaseView struct {
	JobID      string    `json:"job_id"`
	Identifier string    `json:"identifier"`
	Attempt    int       `json:"attempt"`
	WorkerID   string    `json:"worker_id"`
	Expires    time.Time `json:"expires"`
}

// Queued is a job waiting for a worker and why it waits.
type Queued struct {
	Job     Job      `json:"job"`
	Waiting []string `json:"waiting"`
}

// State is a snapshot of workers, queued jobs and leases.
type State struct {
	Workers []Worker    `json:"workers"`
	Queue   []Queued    `json:"queue"`
	Leases  []LeaseView `json:"leases"`
}

// Matcher holds queued jobs and hands each to a worker that polls and
// can run it.
type Matcher struct {
	leaser Leaser
	now    func() time.Time
	tokens Tokens

	mu      sync.Mutex
	workers map[string]*Worker
	pending []Job
	leased  map[string]Job
	ends    map[string]chan Outcome
	stopped map[string]stoppedMachine
}

type stoppedMachine struct {
	worker string
	disk   int64
}

// NewMatcher returns a matcher using leaser, clock now and token issuer
// tokens; a nil tokens selects [NoTokens].
func NewMatcher(leaser Leaser, now func() time.Time, tokens Tokens) *Matcher {
	if tokens == nil {
		tokens = NoTokens{}
	}
	return &Matcher{leaser: leaser, now: now, tokens: tokens,
		workers: map[string]*Worker{}, leased: map[string]Job{},
		ends: map[string]chan Outcome{}, stopped: map[string]stoppedMachine{}}
}

// Submit queues a job and returns the channel its outcome arrives on,
// once.
func (m *Matcher) Submit(j Job) <-chan Outcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.QueuedAt.IsZero() {
		j.QueuedAt = m.now()
	}
	ch := make(chan Outcome, 1)
	m.ends[j.ID] = ch
	m.pending = append(m.pending, j)
	sort.SliceStable(m.pending, func(a, b int) bool { return before(m.pending[a], m.pending[b]) })
	return ch
}

// before orders jobs by priority, a set priority ahead of none, then
// by age.
func before(a, b Job) bool {
	switch {
	case a.Priority != nil && b.Priority == nil:
		return true
	case a.Priority == nil && b.Priority != nil:
		return false
	case a.Priority != nil && *a.Priority != *b.Priority:
		return *a.Priority < *b.Priority
	}
	return a.QueuedAt.Before(b.QueuedAt)
}

// CheckIn records a worker's declaration. What it declares is what it
// has free beyond the jobs it holds here.
func (m *Matcher) CheckIn(id string, d Declaration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := &Worker{ID: id, Declaration: d, Free: d.Resources, LastCheckIn: m.now()}
	for jobID, j := range m.leased {
		if ls := m.leaseOf(jobID); ls.WorkerID == id {
			w.Free = sub(w.Free, j.Needs)
			w.Free.RunSlots--
		}
	}
	for _, s := range m.stopped {
		if s.worker == id {
			w.Free.DiskMiB -= s.disk
		}
	}
	m.workers[id] = w
}

func (m *Matcher) leaseOf(jobID string) Lease {
	for _, ls := range m.leaser.Leases() {
		if ls.JobID == jobID {
			return ls
		}
	}
	return Lease{}
}

func sub(a, b Resources) Resources {
	return Resources{CPUs: a.CPUs - b.CPUs, MemoryMiB: a.MemoryMiB - b.MemoryMiB, DiskMiB: a.DiskMiB - b.DiskMiB, RunSlots: a.RunSlots - b.RunSlots}
}

func add(a, b Resources) Resources {
	return Resources{CPUs: a.CPUs + b.CPUs, MemoryMiB: a.MemoryMiB + b.MemoryMiB, DiskMiB: a.DiskMiB + b.DiskMiB, RunSlots: a.RunSlots + b.RunSlots}
}

// misfits returns why w cannot run j now; none means it can.
func misfits(w *Worker, j Job) []string {
	var out []string
	for _, r := range j.Requires {
		if !slices.Contains(w.Declaration.Capabilities, r) {
			out = append(out, fmt.Sprintf("lacks capability %s", r))
		}
	}
	if w.Free.RunSlots < 1 {
		out = append(out, "no free run slot")
	}
	if w.Free.CPUs < j.Needs.CPUs {
		out = append(out, fmt.Sprintf("%d of %d CPUs free", w.Free.CPUs, j.Needs.CPUs))
	}
	if w.Free.MemoryMiB < j.Needs.MemoryMiB {
		out = append(out, fmt.Sprintf("%d of %d MiB memory free", w.Free.MemoryMiB, j.Needs.MemoryMiB))
	}
	if w.Free.DiskMiB < j.Needs.DiskMiB {
		out = append(out, fmt.Sprintf("%d of %d MiB disk free", w.Free.DiskMiB, j.Needs.DiskMiB))
	}
	return out
}

// Poll offers the worker the first queued job it can run, charging a
// run slot and the job's resources. It returns false when none fits.
func (m *Matcher) Poll(workerID string) (Offer, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[workerID]
	if !ok {
		return Offer{}, false, ErrUnknownWorker
	}
	for i, j := range m.pending {
		if len(misfits(w, j)) > 0 {
			continue
		}
		ls, err := m.leaser.Acquire(j.ID, workerID, m.now())
		if err != nil {
			continue
		}
		toks, err := m.tokens.Mint(j)
		if err != nil {
			_ = m.leaser.Release(j.ID, workerID)
			m.tokens.Void(j.ID)
			return Offer{}, false, fmt.Errorf("worker: minting job tokens: %w", err)
		}
		m.pending = append(m.pending[:i:i], m.pending[i+1:]...)
		m.leased[j.ID] = j
		w.Free = sub(w.Free, j.Needs)
		w.Free.RunSlots--
		flows := make([]Flow, 0, len(j.Flows))
		for _, f := range j.Flows {
			flows = append(flows, Flow{Name: f, Token: toks[f]})
		}
		return Offer{Protocol: Protocol, Job: j, Lease: view(ls, j), Run: j.Run, Flows: flows}, true, nil
	}
	return Offer{}, false, nil
}

func view(ls Lease, j Job) LeaseView {
	return LeaseView{JobID: ls.JobID, Identifier: j.Identifier, Attempt: j.Attempt, WorkerID: ls.WorkerID, Expires: ls.Expires}
}

// held returns the leased job when workerID holds jobID's attempt.
func (m *Matcher) held(jobID string, attempt int) (Job, error) {
	j, ok := m.leased[jobID]
	if !ok || j.Attempt != attempt {
		return Job{}, ErrLeaseLost
	}
	return j, nil
}

// Heartbeat renews the lease on the job's attempt.
func (m *Matcher) Heartbeat(workerID, jobID string, attempt int) (LeaseView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.held(jobID, attempt)
	if err != nil {
		return LeaseView{}, err
	}
	ls, err := m.leaser.Renew(jobID, workerID, m.now())
	if err != nil {
		return LeaseView{}, err
	}
	return view(ls, j), nil
}

// Report ends the job's attempt with the worker's outcome. Its machine
// stops: the run slot, CPUs and memory return, while the disk stays
// held until [Matcher.Remove].
func (m *Matcher) Report(workerID, jobID string, attempt int, out Outcome) error {
	m.mu.Lock()
	j, err := m.held(jobID, attempt)
	if err == nil {
		if ls := m.leaseOf(jobID); ls.WorkerID != workerID || !m.now().Before(ls.Expires) {
			err = ErrLeaseLost
		}
	}
	if err == nil {
		err = m.leaser.Release(jobID, workerID)
	}
	if err != nil {
		m.mu.Unlock()
		if errors.Is(err, ErrUnknownLease) {
			return ErrLeaseLost
		}
		return err
	}
	delete(m.leased, jobID)
	m.tokens.Void(jobID)
	m.stopped[jobID] = stoppedMachine{worker: workerID, disk: j.Needs.DiskMiB}
	if w := m.workers[workerID]; w != nil {
		back := j.Needs
		back.DiskMiB = 0
		back.RunSlots = 1
		w.Free = add(w.Free, back)
	}
	m.endLocked(jobID, out)
	m.mu.Unlock()
	return nil
}

func (m *Matcher) endLocked(jobID string, out Outcome) {
	if ch, ok := m.ends[jobID]; ok {
		delete(m.ends, jobID)
		ch <- out
	}
}

// Remove deletes a stopped machine and frees its disk.
func (m *Matcher) Remove(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.stopped[jobID]; ok {
		delete(m.stopped, jobID)
		if w := m.workers[s.worker]; w != nil {
			w.Free.DiskMiB += s.disk
		}
	}
}

// Cancel withdraws a job, queued or leased, and ends it as cancelled.
// A worker's later heartbeat or report for it is refused.
func (m *Matcher) Cancel(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = slices.DeleteFunc(m.pending, func(j Job) bool { return j.ID == jobID })
	if j, ok := m.leased[jobID]; ok {
		ls := m.leaseOf(jobID)
		_ = m.leaser.Release(jobID, ls.WorkerID)
		delete(m.leased, jobID)
		m.tokens.Void(jobID)
		if w := m.workers[ls.WorkerID]; w != nil {
			back := j.Needs
			back.RunSlots = 1
			w.Free = add(w.Free, back)
		}
	}
	m.endLocked(jobID, Outcome{Cancelled: true, Error: "job withdrawn"})
}

// ExpireLeases ends the attempt of every job whose lease expired and
// returns those jobs. The worker's slot and resources return to its
// pool.
func (m *Matcher) ExpireLeases() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, ls := range m.leaser.Expired(m.now()) {
		j, ok := m.leased[ls.JobID]
		if !ok {
			continue
		}
		delete(m.leased, ls.JobID)
		m.tokens.Void(ls.JobID)
		if w := m.workers[ls.WorkerID]; w != nil {
			back := j.Needs
			back.RunSlots = 1
			w.Free = add(w.Free, back)
		}
		m.endLocked(ls.JobID, Outcome{Expired: true, Error: fmt.Sprintf("lease held by worker %s expired", ls.WorkerID)})
		ids = append(ids, ls.JobID)
	}
	return ids
}

// Snapshot returns the state of workers, queued jobs and leases.
func (m *Matcher) Snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := State{Workers: []Worker{}, Queue: []Queued{}, Leases: []LeaseView{}}
	for _, w := range m.workers {
		s.Workers = append(s.Workers, *w)
	}
	sort.Slice(s.Workers, func(i, j int) bool { return s.Workers[i].ID < s.Workers[j].ID })
	for _, j := range m.pending {
		s.Queue = append(s.Queue, Queued{Job: j, Waiting: m.waitingLocked(j, s.Workers)})
	}
	for _, ls := range m.leaser.Leases() {
		if j, ok := m.leased[ls.JobID]; ok {
			s.Leases = append(s.Leases, view(ls, j))
		}
	}
	return s
}

// waitingLocked says why j waits: per worker, what it lacks.
func (m *Matcher) waitingLocked(j Job, workers []Worker) []string {
	if len(workers) == 0 {
		return []string{"no worker has checked in"}
	}
	var out []string
	for i := range workers {
		if why := misfits(&workers[i], j); len(why) > 0 {
			out = append(out, fmt.Sprintf("worker %s: %s", workers[i].ID, strings.Join(why, ", ")))
		}
	}
	if len(out) == 0 {
		out = []string{"waiting for a worker to poll"}
	}
	return out
}
