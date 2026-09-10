package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/jelly-agent/jelly-agent/internal/engine"
)

// engineRef is one engine plus a count of the work still using it.
//
// A config save swaps the engine (Server.reload). The engine owns the state
// database handle, the search index and the stdio MCP subprocesses, so closing
// it out from under a request that is mid-turn turns "保存配置" into a failed
// answer for whoever happened to be talking at that moment — and the failure
// is a use-after-close deep inside a query, not something the handler can
// report cleanly.
//
// So a replaced engine is *retired* rather than closed: it stops being handed
// out to new work, and it closes when the last thing already using it lets go.
type engineRef struct {
	eng *engine.Engine

	mu      sync.Mutex
	uses    int
	retired bool
}

// acquire pins the engine for one unit of work.
//
// It reports false once the engine has been retired, which tells the caller to
// read the current engine again. That read cannot come back to this ref: the
// swap in reload happens under the write lock and strictly before retire, so a
// reader that gets here late is looking at a pointer that is already stale.
func (r *engineRef) acquire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retired {
		return false
	}
	r.uses++
	return true
}

func (r *engineRef) release() {
	r.mu.Lock()
	r.uses--
	last := r.retired && r.uses == 0
	r.mu.Unlock()
	if last {
		r.eng.Close()
	}
}

// retire takes the engine out of service, and closes it now if nothing is
// using it. Otherwise the last release closes it.
//
// The engine is not closed on a deadline. A request that never returns pins it
// forever — engine, database handle and MCP subprocesses — but a request that
// never returns is already holding far more than this, and a deadline would
// only put back the use-after-close this exists to prevent.
func (r *engineRef) retire() {
	r.mu.Lock()
	if r.retired {
		r.mu.Unlock()
		return
	}
	r.retired = true
	inflight := r.uses
	r.mu.Unlock()
	if inflight == 0 {
		r.eng.Close()
		return
	}
	slog.Info("旧引擎等在途请求跑完再关闭", "在途", inflight)
}

// pin holds the current engine until the returned function is called, which is
// the contract every long-running caller needs: a config save may replace the
// engine meanwhile, but not close this one.
//
// The loop retries rather than failing: acquire only refuses a retired ref, and
// a retired ref means reload has already published its replacement.
func (s *Server) pin() (*engine.Engine, func()) {
	for {
		s.mu.RLock()
		ref := s.ref
		s.mu.RUnlock()
		if ref.acquire() {
			var once sync.Once
			return ref.eng, func() { once.Do(ref.release) }
		}
	}
}

type engineCtxKey struct{}

// pinEngine holds one engine for the lifetime of each request.
//
// One place rather than ~70: every handler reaches the engine from inside a
// request, so pinning at the door covers all of them, including the SSE chat
// stream whose handler does not return until the turn is over.
func (s *Server) pinEngine(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eng, release := s.pin()
		defer release()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), engineCtxKey{}, eng)))
	})
}

// engineFor returns the engine pinned for this request.
//
// This is how a handler must reach anything the engine owns — the state
// database handle, the delivery store, the search index, the tool registry,
// the metrics recorder. s.engine() returns whatever is current *now*, and
// after a config save that is a different engine from the one this request
// was pinned to; the one it started on is guaranteed open until it returns,
// and the current one is not. Two config saves during one request is all it
// takes for the difference to be a closed handle.
//
// s.engine().Config() is the exception and the only one. A *config.Config is
// plain data, handed out by value-semantics and outliving the engine that
// read it, so reading the newest one mid-request is both safe and usually
// what is wanted. TestHandlersReachTheEngineThroughTheRequest enforces the
// line.
//
// Handlers invoked directly by a test have no pinned engine and fall back to
// current.
func (s *Server) engineFor(r *http.Request) *engine.Engine {
	if eng, ok := r.Context().Value(engineCtxKey{}).(*engine.Engine); ok {
		return eng
	}
	return s.engine()
}

// engineAfterReload returns the engine this handler's own save just installed.
//
// The one place a handler must not use its pinned engine. A save that calls
// persist replaces the engine on purpose, and the pinned one is then stale by
// construction — reporting its state back would answer "已关闭" to the request
// that just turned the feature on. What comes back here is read immediately
// and only for what it says about itself, never held.
func (s *Server) engineAfterReload() *engine.Engine { return s.engine() }
