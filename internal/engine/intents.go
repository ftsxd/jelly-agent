package engine

// Remembering what a conversation has been about.
//
// Selection reads one question. A conversation does not stop being about
// metrics just because the third turn is "集群 id 我不清楚，你给我下" — that
// sentence contains no metric word at all, infers nothing, and on its own puts
// the PromQL tools back at the bottom of a flat ranking where the tool budget
// cuts them. The model then reported, correctly, that it had no way to query
// the data source; the capability had been taken away mid-conversation by the
// wording of a follow-up.
//
// So intent accumulates per session, the way admitted tools do. The two work
// on different things and both are needed: admission keeps a tool that already
// entered the prompt, and cannot help a conversation whose every turn worded
// itself out of the tools; carried intent gets them in.
//
// Accumulate-only, never forgotten within a session. A conversation that moves
// from metrics to logs keeps both, and that is right — it can move back, and
// this turn's own matches are admitted first anyway, so the carried half only
// competes for what is left.

import (
	"sync"

	"github.com/jelly-agent/jelly-agent/internal/selector"
)

// maxIntentSessions bounds the bookkeeping, like admissions. Evicting an entry
// costs that conversation its accumulated intent — the next turn starts from
// its own wording again — which is a degradation, not a fault.
const maxIntentSessions = 512

type intents struct {
	mu    sync.Mutex
	byID  map[string]selector.Intent
	order []string
}

func newIntents() *intents { return &intents{byID: map[string]selector.Intent{}} }

// carry folds this turn's intent into the session's and returns the union.
//
// The union rather than the stored value, so a first turn behaves exactly as
// it did before this existed: nothing remembered yet, nothing added.
func (r *intents) carry(session string, now selector.Intent) selector.Intent {
	if session == "" {
		return now // nothing to be continuous across
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	prev, seen := r.byID[session]
	if !seen {
		if len(r.byID) >= maxIntentSessions {
			r.evictLocked()
		}
		r.order = append(r.order, session)
	}
	merged := prev.Merge(now)
	r.byID[session] = merged
	return merged
}

// evictLocked drops the oldest session. Callers hold the lock.
func (r *intents) evictLocked() {
	if len(r.order) == 0 {
		return
	}
	oldest := r.order[0]
	r.order = r.order[1:]
	delete(r.byID, oldest)
}

// intents returns the process-wide record.
//
// One per engine, for the same reason as admissions: it is keyed by session,
// and the web server rebuilds the agent between turns of one conversation.
func (e *Engine) intents() *intents {
	e.intentOnce.Do(func() { e.intent = newIntents() })
	return e.intent
}
