package server

import (
	"container/list"
	"fmt"
	"sync"
)

// frameCache remembers the projection of a session's events.
//
// The task list walks a page of sessions and projects each one, and projecting
// means loading every event of that session through ADK's service — which has
// no batch API. Measured against PostgreSQL: 20.3ms per session, 24 sessions
// to fill one page, so a page cost 642ms and the worst path (a filter matching
// nothing, taskScanMax = 600) would cost twelve seconds.
//
// What is cached is the expensive half only: the events, projected to frames.
// Deliberately not the tasks. Folding frames into tasks needs the tool
// metadata and the task links, and both change without the session changing —
// so a cache holding tasks would need to know when metadata was edited, and a
// cache that has to be told about a second kind of change is a cache whose
// invalidation nobody can reason about. Frames depend on the events and
// nothing else.
type frameCache struct {
	mu    sync.Mutex
	byKey map[string]*list.Element
	order *list.List
	max   int
}

type frameEntry struct {
	key    string
	frames []map[string]any
}

// frameCacheSize bounds it. The scan ceiling is 600 sessions, so this holds
// several full worst-case scans; it exists so a long-running server cannot
// accumulate one entry per session ever created.
const frameCacheSize = 2048

func newFrameCache() *frameCache {
	return &frameCache{byKey: map[string]*list.Element{}, order: list.New(), max: frameCacheSize}
}

// frameKey identifies one version of a session's event log.
//
// The update time alone is not enough: ListPage reports it in whole seconds,
// and two events a few hundred milliseconds apart share one. The event count
// closes that — ADK appends and never rewrites, so a log that has changed has
// either a later timestamp or more events in it.
func frameKey(sessionID string, lastUpdate int64, events int) string {
	return fmt.Sprintf("%s\x00%d\x00%d", sessionID, lastUpdate, events)
}

// get returns the cached frames, or nil.
func (c *frameCache) get(key string) ([]map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*frameEntry).frames, true
}

func (c *frameCache) put(key string, frames []map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[key]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.byKey[key] = c.order.PushFront(&frameEntry{key: key, frames: frames})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byKey, oldest.Value.(*frameEntry).key)
	}
}

// forget drops every version of these sessions, for a delete.
//
// By prefix, because the key carries a version this caller does not know. A
// deleted session must not be answerable from any of them.
func (c *frameCache) forget(ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		prefix := id + "\x00"
		for el := c.order.Front(); el != nil; {
			next := el.Next()
			if e := el.Value.(*frameEntry); len(e.key) > len(prefix) && e.key[:len(prefix)] == prefix {
				c.order.Remove(el)
				delete(c.byKey, e.key)
			}
			el = next
		}
	}
}

func (s *Server) frames() *frameCache {
	s.frameOnce.Do(func() { s.frameLRU = newFrameCache() })
	return s.frameLRU
}

// held counts the cached projections of one session, for tests. A delete must
// leave none — see the purge note on forget.
func (c *frameCache) held(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := id + "\x00"
	n := 0
	for el := c.order.Front(); el != nil; el = el.Next() {
		if k := el.Value.(*frameEntry).key; len(k) > len(prefix) && k[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}
