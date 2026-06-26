package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"
)

// TestListPageAndAllIDs verifies the paginated projection: newest-first order,
// total count, limit/offset paging, real per-session event counts (which ADK's
// own List does not surface), and the id-only AllIDs helper.
func TestListPageAndAllIDs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	svc, err := NewSQLite(dbPath)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	ctx := context.Background()
	const app, user = "jelly-agent", "local-user"

	// Create s1..s5 with distinct update_times (s5 newest). Append 3 events to
	// s5 so its count is non-zero; s5 stays newest since it was created last.
	for i := 1; i <= 5; i++ {
		sid := fmt.Sprintf("s%d", i)
		resp, err := svc.Create(ctx, &adksession.CreateRequest{AppName: app, UserID: user, SessionID: sid})
		if err != nil {
			t.Fatalf("Create %s: %v", sid, err)
		}
		if sid == "s5" {
			for j := 0; j < 3; j++ {
				ev := adksession.NewEvent("inv")
				ev.Author = "user"
				ev.Content = genai.NewContentFromText("hi", genai.RoleUser)
				if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
					t.Fatalf("AppendEvent: %v", err)
				}
			}
		}
		time.Sleep(3 * time.Millisecond) // ensure strictly increasing update_time
	}

	page, total, err := ListPage(dbPath, app, user, 2, 0)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if total != 5 {
		t.Fatalf("total=%d, want 5", total)
	}
	if len(page) != 2 {
		t.Fatalf("page len=%d, want 2", len(page))
	}
	if page[0].ID != "s5" || page[1].ID != "s4" {
		t.Errorf("order=[%q,%q], want [s5,s4]", page[0].ID, page[1].ID)
	}
	if page[0].Events != 3 {
		t.Errorf("s5 events=%d, want 3", page[0].Events)
	}
	if page[0].LastUpdate <= 0 {
		t.Errorf("s5 last_update=%d, want > 0", page[0].LastUpdate)
	}

	// Second page continues the order without overlap.
	page2, _, err := ListPage(dbPath, app, user, 2, 2)
	if err != nil {
		t.Fatalf("ListPage page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != "s3" || page2[1].ID != "s2" {
		t.Errorf("page2=%+v, want s3,s2", page2)
	}

	// AllIDs returns every id, newest first.
	ids, err := AllIDs(dbPath, app, user)
	if err != nil {
		t.Fatalf("AllIDs: %v", err)
	}
	want := []string{"s5", "s4", "s3", "s2", "s1"}
	if len(ids) != len(want) {
		t.Fatalf("AllIDs len=%d, want %d (%v)", len(ids), len(want), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("AllIDs[%d]=%q, want %q", i, ids[i], want[i])
		}
	}
}
