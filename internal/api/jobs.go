package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Job states, in the order a job moves through them. A job that never acquires
// its alias's lock stays queued until it does.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// DefaultJobRetention is how long a finished job's result stays available.
//
// Async jobs exist so an HTTP client can stop waiting; the result still has to
// be fetchable long enough to matter, without jobs accumulating for the life of
// the process.
const DefaultJobRetention = 10 * time.Minute

// Job is the observable state of an asynchronous deploy or upgrade.
//
// Jobs live in memory only: they survive the HTTP request that created them,
// but a restart loses every unfinished and finished job. That is deliberate —
// durable job state would mean another table and a recovery protocol, and the
// on-chain result plus the deployment history already survive.
type Job struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Alias   string `json:"alias"`
	Network string `json:"network,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Result is the same object the synchronous endpoint returns, present once
	// State is succeeded.
	Result any `json:"result,omitempty"`

	// Error is the failure message, present once State is failed.
	Error string `json:"error,omitempty"`
}

// jobSpec is one unit of queued work.
type jobSpec struct {
	alias   string
	network string
	run     func(ctx context.Context) (any, error)
}

// jobManager owns the in-memory jobs and the per-alias queueing that keeps two
// operations on one contract from running at the same time.
type jobManager struct {
	mu        sync.Mutex
	jobs      map[string]*Job
	aliases   *keyedMutex
	retention time.Duration
	log       *slog.Logger
}

func newJobManager(retention time.Duration, log *slog.Logger) *jobManager {
	if retention <= 0 {
		retention = DefaultJobRetention
	}
	return &jobManager{
		jobs:      map[string]*Job{},
		aliases:   newKeyedMutex(),
		retention: retention,
		log:       log,
	}
}

// submit records a queued job and starts its worker. It returns a snapshot, so
// the caller can encode it without racing the worker.
func (m *jobManager) submit(spec jobSpec) (*Job, error) {
	id, err := newJobID()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	job := &Job{
		ID:        id,
		State:     JobQueued,
		Alias:     spec.alias,
		Network:   spec.network,
		CreatedAt: now,
	}

	m.mu.Lock()
	m.pruneLocked(now)
	m.jobs[id] = job
	snapshot := *job
	m.mu.Unlock()

	go m.run(id, spec)

	return &snapshot, nil
}

// get returns a snapshot of a job, pruning expired ones first.
func (m *jobManager) get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pruneLocked(time.Now())
	job, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *job, true
}

// run executes one job: wait for its alias's turn, then run with a detached
// context.
//
// The context is deliberately not the HTTP request's: the whole point of an
// async job is that it outlives the connection that asked for it.
func (m *jobManager) run(id string, spec jobSpec) {
	// A panic here is outside net/http's Recoverer and would take the server
	// down; a broken job must fail alone.
	defer func() {
		if recovered := recover(); recovered != nil {
			m.finish(id, nil, fmt.Errorf("job panicked: %v", recovered))
		}
	}()

	release := m.aliases.acquire(spec.alias)
	defer release()

	started := time.Now().UTC()
	m.markRunning(id, started)

	log := m.log.With("job_id", id, "alias", spec.alias, "network", spec.network)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	result, err := spec.run(ctx)
	m.finish(id, result, err)

	if err != nil {
		log.Error("async job failed", "error", err, "duration", time.Since(started))
		return
	}
	log.Info("async job succeeded", "duration", time.Since(started))
}

func (m *jobManager) markRunning(id string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if job, ok := m.jobs[id]; ok {
		job.State = JobRunning
		job.StartedAt = &at
	}
}

func (m *jobManager) finish(id string, result any, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, ok := m.jobs[id]
	if !ok {
		return
	}

	finished := time.Now().UTC()
	job.FinishedAt = &finished
	if err != nil {
		job.State = JobFailed
		job.Error = err.Error()
		return
	}
	job.State = JobSucceeded
	job.Result = result
}

// pruneLocked drops finished jobs older than the retention window. Called with
// m.mu held.
func (m *jobManager) pruneLocked(now time.Time) {
	for id, job := range m.jobs {
		if job.FinishedAt != nil && now.Sub(*job.FinishedAt) >= m.retention {
			delete(m.jobs, id)
		}
	}
}

// newJobID returns an unguessable job identifier. It is not a capability — the
// endpoint still requires the API token — but a guessable ID would make
// accidental collisions between concurrent callers possible.
func newJobID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// keyedMutex is a mutex per string key, released and reclaimed when the last
// holder leaves. Alias names are bounded by the config, but a caller can ask
// for a nonexistent alias, so entries are not allowed to accumulate.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sem     chan struct{}
	waiters int
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: map[string]*keyedLock{}}
}

// acquire blocks until the key is free and returns a release function.
func (m *keyedMutex) acquire(key string) func() {
	m.mu.Lock()
	lock, ok := m.locks[key]
	if !ok {
		lock = &keyedLock{sem: make(chan struct{}, 1)}
		m.locks[key] = lock
	}
	lock.waiters++
	m.mu.Unlock()

	lock.sem <- struct{}{}

	return func() {
		<-lock.sem

		m.mu.Lock()
		lock.waiters--
		if lock.waiters == 0 {
			delete(m.locks, key)
		}
		m.mu.Unlock()
	}
}
