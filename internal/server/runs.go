package server

// Which runs are happening right now.
//
// A finished run can be read off its stored events; a running one cannot,
// because "running" is not a fact about the events — it is a fact about this
// process. An invocation whose events stop is indistinguishable from one that
// was abandoned, and from one that is still thinking, unless something is
// keeping track.
//
// So this is in memory, on purpose, and it is empty after a restart. That is
// the accurate answer rather than a limitation: nothing is running after a
// restart, and a task that was running when the process died was, in fact,
// cancelled. Persisting "running" would produce a task that claims to be in
// progress with nothing driving it — a lie that outlives every restart.

import (
	"sync"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/task"
)

// maxTrackedRuns bounds the bookkeeping. The map only holds in-flight runs, so
// it is small in practice; the cap is there because an entry leaks if a
// handler dies between start and finish.
const maxTrackedRuns = 256

// runOutcome is what a finished run ended as, kept briefly so a list that
// arrives moments later does not report a just-finished task as cancelled.
type runOutcome struct {
	status string
	at     time.Time
}

// outcomeTTL is how long a finished run's verdict is remembered. Longer than
// any list refresh, far shorter than anything that would count as state.
const outcomeTTL = 2 * time.Minute

type runRegistry struct {
	mu sync.Mutex
	// running counts the runs in flight for a task, not whether one is.
	//
	// A task id can have more than one at a time: two browsers continuing the
	// same task, or a scheduled trigger landing while somebody is following up
	// by hand. With a single entry per task the second registration overwrote
	// the first and whichever finished first deleted the only record — so the
	// task went to completed while the other run was still working, which is
	// the failure mode that looks like a finished answer.
	running  map[string]*taskRuns
	outcomes map[string]runOutcome // task id → how the last one ended
}

// taskRuns is one task's in-flight runs, by the invocation each is.
//
// Keyed by invocation rather than counted, so a finish that arrives twice —
// the handler's own call and the deferred safety net — removes one run rather
// than two.
type taskRuns struct {
	started map[string]time.Time
	first   time.Time // for eviction: the oldest thing this entry is holding
}

func newRunRegistry() *runRegistry {
	return &runRegistry{running: map[string]*taskRuns{}, outcomes: map[string]runOutcome{}}
}

// start marks a run in flight and returns the function that ends it.
//
// The returned function takes the outcome, so the caller states it rather than
// this package guessing from a nil error — a stream that ended because the
// browser went away is cancelled, not completed, and only the caller knows.
//
// taskID is which task the status belongs to, and it is not always the run's
// own: a run joined to an earlier task is displayed under that task's id, so
// registering it under session/its-own-invocation put the status somewhere
// nothing looks. The continuation of a task showed as idle while it ran, then
// as whatever the first run had ended as. Empty means the run is its own task.
func (r *runRegistry) start(session, round, taskID string) func(status string) {
	id := taskID
	if id == "" {
		id = task.ID(session, round)
	}
	now := time.Now()
	r.mu.Lock()
	if len(r.running) >= maxTrackedRuns {
		r.evictLocked()
	}
	e, ok := r.running[id]
	if !ok {
		e = &taskRuns{started: map[string]time.Time{}, first: now}
		r.running[id] = e
	}
	e.started[round] = now
	r.mu.Unlock()

	// Idempotent by construction rather than by a guard: the run is removed by
	// its invocation, so a second call for the same run removes nothing. The
	// handler calls this and a deferred safety net calls it again on any path
	// that skipped the first, and neither may retire somebody else's run.
	return func(status string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		e, ok := r.running[id]
		if !ok {
			return
		}
		delete(e.started, round)
		// The outcome is recorded whichever run reported it, but the task only
		// stops being live when the last of them has gone. A task with work
		// still in flight is running, whatever the run that just ended has to
		// say about itself.
		r.outcomes[id] = runOutcome{status: status, at: time.Now()}
		if len(e.started) == 0 {
			delete(r.running, id)
		}
	}
}

// status reports what is known about a task, and whether anything is.
func (r *runRegistry) status(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, live := r.running[id]; live && len(e.started) > 0 {
		return TaskRunning, true
	}
	if o, ok := r.outcomes[id]; ok && time.Since(o.at) < outcomeTTL {
		return o.status, true
	}
	return "", false
}

// evictIfFull drops the oldest entries. Callers hold the lock.
func (r *runRegistry) evictLocked() {
	oldestID, oldest := "", time.Now()
	for id, e := range r.running {
		if e.first.Before(oldest) {
			oldestID, oldest = id, e.first
		}
	}
	if oldestID != "" {
		delete(r.running, oldestID)
	}
	cutoff := time.Now().Add(-outcomeTTL)
	for id, o := range r.outcomes {
		if o.at.Before(cutoff) {
			delete(r.outcomes, id)
		}
	}
}

// runs returns the process-wide registry.
func (s *Server) runs() *runRegistry {
	s.runsOnce.Do(func() { s.runReg = newRunRegistry() })
	return s.runReg
}
