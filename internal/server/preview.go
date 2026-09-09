package server

import (
	"container/list"
	"context"
	"strings"
	"sync"

	adksession "google.golang.org/adk/session"

	"github.com/jelly-agent/jelly-agent/internal/engine"
)

// previewCache remembers each session's first user message.
//
// The sessions page shows one line per session, and getting that line meant
// loading the whole session — every event of it — through ADK's service, once
// per row. Twenty rows of thirty events each, to display twenty short strings.
// Free enough against a local SQLite file to stay invisible; 480ms against
// PostgreSQL, which is what made it visible.
//
// Cacheable because the answer cannot change: it is the first user message of
// a conversation, fixed the moment that message is written. A session that has
// none yet is cached as empty and asked again — that one does change, exactly
// once, when the conversation starts.
type previewCache struct {
	mu    sync.Mutex
	byID  map[string]*list.Element
	order *list.List // front is newest; evicted from the back
	max   int
}

type previewEntry struct {
	id   string
	text string
}

// previewCacheSize bounds it. Sessions are shown a page at a time and the
// pages a person walks through are few, so this holds far more than a working
// set; it exists so a long-running server cannot accumulate one entry per
// session ever created.
const previewCacheSize = 2048

func newPreviewCache() *previewCache {
	return &previewCache{
		byID:  map[string]*list.Element{},
		order: list.New(),
		max:   previewCacheSize,
	}
}

// get returns the cached preview, or loads and caches it.
//
// An empty result is not cached: it means the session has no user message yet,
// which is a state it leaves. Anything else is final.
func (c *previewCache) get(ctx context.Context, svc adksession.Service, id string) string {
	c.mu.Lock()
	if el, ok := c.byID[id]; ok {
		c.order.MoveToFront(el)
		text := el.Value.(*previewEntry).text
		c.mu.Unlock()
		return text
	}
	c.mu.Unlock()

	text := loadPreview(ctx, svc, id)
	if text == "" {
		return ""
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[id]; ok { // another request got there first
		c.order.MoveToFront(el)
		return el.Value.(*previewEntry).text
	}
	c.byID[id] = c.order.PushFront(&previewEntry{id: id, text: text})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byID, oldest.Value.(*previewEntry).id)
	}
	return text
}

// forget drops sessions the cache must not answer for any more.
func (c *previewCache) forget(ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if el, ok := c.byID[id]; ok {
			c.order.Remove(el)
			delete(c.byID, id)
		}
	}
}

// loadPreview reads the first user text. Best-effort: a missing or corrupt
// session must not make the list unusable.
func loadPreview(ctx context.Context, svc adksession.Service, id string) string {
	resp, err := svc.Get(ctx, &adksession.GetRequest{
		AppName: engine.AppName, UserID: engine.UserID, SessionID: id,
	})
	if err != nil || resp.Session == nil {
		return ""
	}
	for ev := range resp.Session.Events().All() {
		if roleForAuthor(ev.Author) != "user" || ev.Content == nil {
			continue
		}
		var b strings.Builder
		for _, p := range ev.Content.Parts {
			if p != nil {
				b.WriteString(p.Text)
			}
		}
		text := strings.TrimSpace(b.String())
		if len([]rune(text)) > 42 {
			return string([]rune(text)[:42]) + "…"
		}
		if text != "" {
			return text
		}
	}
	return ""
}

func (s *Server) previews() *previewCache {
	s.previewOnce.Do(func() { s.previewLRU = newPreviewCache() })
	return s.previewLRU
}
