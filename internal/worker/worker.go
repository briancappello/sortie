// Package worker matches jobs to workers that dial out to Sortie. A
// worker checks in with its capabilities and free resources, polls for
// a job, and holds a lease on it that it must renew. The rule that
// decides who holds a lease sits behind [Leaser].
package worker

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Errors reported by the matcher and the lease rule.
var (
	ErrUnknownWorker = errors.New("worker: unknown worker")
	ErrUnknownLease  = errors.New("worker: unknown lease")
	ErrLeaseHeld     = errors.New("worker: job already leased")
	ErrLeaseLost     = errors.New("worker: lease expired or held by another worker")
)

// Resources are what a worker has free.
type Resources struct {
	RunSlots int   `json:"run_slots"`
	DiskMB   int64 `json:"disk_mb"`
}

// Worker is a checked-in worker.
type Worker struct {
	ID           string    `json:"id"`
	Capabilities []string  `json:"capabilities"`
	Free         Resources `json:"free"`
	LastSeen     time.Time `json:"last_seen"`
}

// Job is a run handed to the matcher by the remote environment kind.
type Job struct {
	ID string `json:"id"`
	// Requires lists capabilities a worker must all have.
	Requires []string `json:"requires"`
	// DiskMB is the disk the job's machine needs.
	DiskMB int64 `json:"disk_mb"`
}

// Lease is the right of one worker to run one job until Expires.
type Lease struct {
	JobID    string    `json:"job_id"`
	WorkerID string    `json:"worker_id"`
	Expires  time.Time `json:"expires"`
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
	TTL    time.Duration
	mu     sync.Mutex
	leases map[string]Lease
}

// NewTTLLeaser returns a leaser whose leases last ttl.
func NewTTLLeaser(ttl time.Duration) *TTLLeaser {
	return &TTLLeaser{TTL: ttl, leases: map[string]Lease{}}
}

// Acquire implements [Leaser].
func (l *TTLLeaser) Acquire(jobID, workerID string, now time.Time) (Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.leases[jobID]; ok && now.Before(cur.Expires) {
		return Lease{}, ErrLeaseHeld
	}
	ls := Lease{JobID: jobID, WorkerID: workerID, Expires: now.Add(l.TTL)}
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
	cur.Expires = now.Add(l.TTL)
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

// State is a snapshot of workers and leases for the state API.
type State struct {
	Workers []Worker `json:"workers"`
	Pending []Job    `json:"pending"`
	Leases  []Lease  `json:"leases"`
}

// Matcher holds pending jobs and hands each to a worker that polls and
// can run it.
type Matcher struct {
	leaser Leaser
	now    func() time.Time

	mu      sync.Mutex
	workers map[string]*Worker
	pending []Job
	leased  map[string]Job
	stopped map[string]int64 // job -> disk held by a stopped machine
	onEnd   func(jobID string)
}

// NewMatcher returns a matcher using leaser and clock now. onEnd, when
// non-nil, is called with the job of every lease that expired, so the
// attempt ends.
func NewMatcher(leaser Leaser, now func() time.Time, onEnd func(jobID string)) *Matcher {
	return &Matcher{leaser: leaser, now: now, onEnd: onEnd,
		workers: map[string]*Worker{}, leased: map[string]Job{}, stopped: map[string]int64{}}
}

// Submit queues a job.
func (m *Matcher) Submit(j Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, j)
}

// CheckIn records a worker's capabilities and free resources.
func (m *Matcher) CheckIn(w Worker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.LastSeen = m.now()
	m.workers[w.ID] = &w
}

func capable(w *Worker, j Job) bool {
	have := make(map[string]bool, len(w.Capabilities))
	for _, c := range w.Capabilities {
		have[c] = true
	}
	for _, r := range j.Requires {
		if !have[r] {
			return false
		}
	}
	return w.Free.RunSlots >= 1 && w.Free.DiskMB >= j.DiskMB
}

// Poll leases the first pending job the worker can run, charging a run
// slot and the job's disk. It returns false when none fits.
func (m *Matcher) Poll(workerID string) (Job, Lease, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[workerID]
	if !ok {
		return Job{}, Lease{}, false, ErrUnknownWorker
	}
	for i, j := range m.pending {
		if !capable(w, j) {
			continue
		}
		ls, err := m.leaser.Acquire(j.ID, workerID, m.now())
		if err != nil {
			continue
		}
		m.pending = append(m.pending[:i:i], m.pending[i+1:]...)
		m.leased[j.ID] = j
		w.Free.RunSlots--
		w.Free.DiskMB -= j.DiskMB
		return j, ls, true, nil
	}
	return Job{}, Lease{}, false, nil
}

// Renew extends a lease.
func (m *Matcher) Renew(jobID, workerID string) (Lease, error) {
	return m.leaser.Renew(jobID, workerID, m.now())
}

// Stop ends the job's run: the lease drops and the run slot is freed,
// while a stopped machine keeps its disk.
func (m *Matcher) Stop(jobID, workerID string) error {
	if err := m.leaser.Release(jobID, workerID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.leased[jobID]
	delete(m.leased, jobID)
	m.stopped[jobID] = j.DiskMB
	if w := m.workers[workerID]; w != nil {
		w.Free.RunSlots++
	}
	return nil
}

// Remove deletes a stopped machine and frees its disk.
func (m *Matcher) Remove(jobID, workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.stopped[jobID]; ok {
		delete(m.stopped, jobID)
		if w := m.workers[workerID]; w != nil {
			w.Free.DiskMB += d
		}
	}
}

// ExpireLeases ends the attempt of every job whose lease expired and
// returns those jobs. The worker's slot and disk return to its pool.
func (m *Matcher) ExpireLeases() []string {
	exp := m.leaser.Expired(m.now())
	m.mu.Lock()
	var ids []string
	for _, ls := range exp {
		j := m.leased[ls.JobID]
		delete(m.leased, ls.JobID)
		if w := m.workers[ls.WorkerID]; w != nil {
			w.Free.RunSlots++
			w.Free.DiskMB += j.DiskMB
		}
		ids = append(ids, ls.JobID)
	}
	m.mu.Unlock()
	if m.onEnd != nil {
		for _, id := range ids {
			m.onEnd(id)
		}
	}
	return ids
}

// Snapshot returns the state of workers, pending jobs and leases.
func (m *Matcher) Snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := State{Pending: append([]Job{}, m.pending...), Leases: m.leaser.Leases(), Workers: []Worker{}}
	for _, w := range m.workers {
		s.Workers = append(s.Workers, *w)
	}
	sort.Slice(s.Workers, func(i, j int) bool { return s.Workers[i].ID < s.Workers[j].ID })
	return s
}
