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
	mu       sync.Mutex
	running  map[string]time.Time  // task id → started
	outcomes map[string]runOutcome // task id → how it ended
}

func newRunRegistry() *runRegistry {
	return &runRegistry{running: map[string]time.Time{}, outcomes: map[string]runOutcome{}}
}

// start marks a run in flight and returns the function that ends it.
//
// The returned function takes the outcome, so the caller states it rather than
// this package guessing from a nil error — a stream that ended because the
// browser went away is cancelled, not completed, and only the caller knows.
func (r *runRegistry) start(session, round string) func(status string) {
	id := task.ID(session, round)
	r.mu.Lock()
	if len(r.running) >= maxTrackedRuns {
		r.evictLocked()
	}
	r.running[id] = time.Now()
	r.mu.Unlock()

	return func(status string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.running, id)
		r.outcomes[id] = runOutcome{status: status, at: time.Now()}
	}
}

// status reports what is known about a task, and whether anything is.
func (r *runRegistry) status(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, live := r.running[id]; live {
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
	for id, at := range r.running {
		if at.Before(oldest) {
			oldestID, oldest = id, at
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
