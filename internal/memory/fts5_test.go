package memory

import (
	"context"
	"path/filepath"
	"testing"

	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	jellysession "github.com/jelly-agent/jelly-agent/internal/session"
)

const (
	testApp  = "jelly-agent"
	testUser = "local-user"
)

// newTestSearch opens a Search on a throwaway state.db under t.TempDir.
func newTestSearch(t *testing.T) *Search {
	t.Helper()
	s, err := NewSearch(filepath.Join(t.TempDir(), "state.db"), 5)
	if err != nil {
		t.Fatalf("NewSearch: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// buildSession creates an in-memory session populated with the given events,
// each a (author, text) pair. An empty text yields a function-call-only event
// (no indexable text), to exercise the skip path.
func buildSession(t *testing.T, id string, events [][2]string) session.Session {
	t.Helper()
	svc := session.InMemoryService()
	ctx := context.Background()
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: testApp, UserID: testUser, SessionID: id})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sess := resp.Session
	for i, e := range events {
		author, text := e[0], e[1]
		var content *genai.Content
		if text == "" {
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: "web_search", Args: map[string]any{"query": "x"}},
			}}}
		} else {
			content = genai.NewContentFromText(text, genai.Role(roleFor(author)))
		}
		ev := &session.Event{
			LLMResponse: model.LLMResponse{Content: content},
			ID:          id + "-" + string(rune('a'+i)),
			Author:      author,
		}
		if err := svc.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
	return sess
}

func search(t *testing.T, s *Search, query string) []adkmemory.Entry {
	t.Helper()
	resp, err := s.SearchMemory(context.Background(), &adkmemory.SearchRequest{
		Query: query, AppName: testApp, UserID: testUser,
	})
	if err != nil {
		t.Fatalf("SearchMemory(%q): %v", query, err)
	}
	return resp.Memories
}

func TestSearchRoundTrip(t *testing.T) {
	s := newTestSearch(t)
	sess := buildSession(t, "s1", [][2]string{
		{"user", "我最喜欢的饮料是燕麦拿铁咖啡"},
		{"root", "好的，已记住你喜欢燕麦拿铁。"},
		{"root", ""}, // function-call-only event: must be skipped
	})
	if err := s.AddSessionToMemory(context.Background(), sess); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}

	// Chinese substring (≥3 runes) via trigram MATCH.
	if got := search(t, s, "燕麦拿铁"); len(got) == 0 {
		t.Fatalf("expected a match for 燕麦拿铁, got none")
	}

	// The skipped function-call event should not appear: only the two text
	// events were indexed, and a broad query matching both returns at most 2.
	got := search(t, s, "燕麦")
	if len(got) != 2 {
		t.Fatalf("expected 2 indexed text events, got %d", len(got))
	}
	for _, e := range got {
		if e.Content == nil || len(e.Content.Parts) == 0 || e.Content.Parts[0].Text == "" {
			t.Errorf("entry has no text content: %+v", e)
		}
	}
}

func TestSearchShortQueryFallback(t *testing.T) {
	s := newTestSearch(t)
	sess := buildSession(t, "s1", [][2]string{{"user", "我爱喝茶"}})
	if err := s.AddSessionToMemory(context.Background(), sess); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}
	// "茶" is a single rune (<3) → LIKE fallback path.
	if got := search(t, s, "茶"); len(got) != 1 {
		t.Fatalf("short-query fallback: expected 1 match, got %d", len(got))
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	s := newTestSearch(t)
	sess := buildSession(t, "s1", [][2]string{{"user", "hello world"}})
	if err := s.AddSessionToMemory(context.Background(), sess); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}
	if got := search(t, s, "   "); len(got) != 0 {
		t.Fatalf("empty query should return nothing, got %d", len(got))
	}
}

func TestSearchScoping(t *testing.T) {
	s := newTestSearch(t)
	sess := buildSession(t, "s1", [][2]string{{"user", "alpha beta gamma"}})
	if err := s.AddSessionToMemory(context.Background(), sess); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}
	// Different user must not see another user's memory.
	resp, err := s.SearchMemory(context.Background(), &adkmemory.SearchRequest{
		Query: "alpha", AppName: testApp, UserID: "someone-else",
	})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("cross-user leak: expected 0, got %d", len(resp.Memories))
	}
}

func TestAddSessionIdempotent(t *testing.T) {
	s := newTestSearch(t)
	sess := buildSession(t, "s1", [][2]string{
		{"user", "alpha beta"},
		{"root", "alpha gamma"},
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.AddSessionToMemory(ctx, sess); err != nil {
			t.Fatalf("AddSessionToMemory #%d: %v", i, err)
		}
	}
	// Re-adding the same session must replace, not duplicate: still 2 rows.
	if got := search(t, s, "alpha"); len(got) != 2 {
		t.Fatalf("idempotent re-add: expected 2 rows, got %d", len(got))
	}
}

// TestSearchCoexistsWithSessionDB exercises the real layout: the FTS5 index and
// the GORM-backed session store share one state.db file via separate
// connections (PLAN §10.3). It verifies the FTS5 table is created alongside the
// migrated session schema and that a session persisted through the session
// service can be ingested and searched.
func TestSearchCoexistsWithSessionDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()

	// Open and migrate the ADK session store on the shared file first.
	svc, err := jellysession.NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: testApp, UserID: testUser, SessionID: "s1"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ev := &session.Event{
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("我住在杭州西湖区", genai.RoleUser)},
		ID:          "e1",
		Author:      "user",
	}
	if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("append event: %v", err)
	}

	// Now open the FTS5 index on the same file via its own connection.
	s, err := NewSearch(dbPath, 5)
	if err != nil {
		t.Fatalf("NewSearch on shared db: %v", err)
	}
	defer s.Close()

	// Re-read the persisted session and ingest it.
	got, err := svc.Get(ctx, &session.GetRequest{AppName: testApp, UserID: testUser, SessionID: "s1"})
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if err := s.AddSessionToMemory(ctx, got.Session); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}
	if hits := search(t, s, "西湖区"); len(hits) != 1 {
		t.Fatalf("expected 1 hit from shared-db ingest, got %d", len(hits))
	}
}

func TestSearchQuotedQueryNoSyntaxError(t *testing.T) {
	s := newTestSearch(t)
	sess := buildSession(t, "s1", [][2]string{{"user", `he said "quote" and (parens) OR stuff`}})
	if err := s.AddSessionToMemory(context.Background(), sess); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}
	// A query full of FTS5 operator characters must not error (it is quoted).
	if got := search(t, s, `"quote" AND (parens)`); len(got) == 0 {
		t.Fatalf("expected the literal substring to match, got none")
	}
}
