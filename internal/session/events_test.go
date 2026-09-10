package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

const (
	evApp  = "jelly-agent"
	evUser = "local-user"
)

// What ADK writes, read back by EventsOf, must be what ADK reads back.
//
// EventsOf goes to the table directly, because ADK's service has no batch API
// and one Get per session is the cost this exists to remove. That is only
// safe while the two agree about the stored shape — so this writes through
// ADK and compares field by field with what ADK's own Get returns.
//
// It is here to fail on an ADK upgrade that changes how a column is written,
// which is the failure mode of reading somebody else's table.
func TestBatchReadMatchesWhatADKReadsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	svc, closeSvc, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSvc()

	const id = "web-1"
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: evApp, UserID: evUser, SessionID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	// One of each shape the projection reads: text, a tool call, a tool
	// result, an agent transfer, and a model error.
	base := time.Now().UTC().Truncate(time.Millisecond)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	want := []*adksession.Event{
		{
			ID: "e1", Author: "user", InvocationID: "inv-1", Branch: "root",
			Timestamp:   at(1),
			LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("排查一下", genai.RoleUser)},
		},
		{
			ID: "e2", Author: "model", InvocationID: "inv-1",
			Timestamp: at(2),
			LLMResponse: adkmodel.LLMResponse{
				Content: &genai.Content{Role: "model", Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{ID: "c1", Name: "get_logs",
						Args: map[string]any{"svc": "payment"}},
				}}},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount: 90, CandidatesTokenCount: 8, TotalTokenCount: 98,
				},
			},
		},
		{
			ID: "e3", Author: "user", InvocationID: "inv-1",
			Timestamp: at(3),
			LLMResponse: adkmodel.LLMResponse{
				Content: &genai.Content{Role: "user", Parts: []*genai.Part{{
					FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "get_logs",
						Response: map[string]any{"summary": "命中 3 行", "evidence_id": "e1"}},
				}}},
			},
		},
		{
			ID: "e4", Author: "root", InvocationID: "inv-1",
			Timestamp: at(4),
			Actions:   adksession.EventActions{TransferToAgent: "MetricsQuery"},
		},
		{
			ID: "e5", Author: "model", InvocationID: "inv-1",
			Timestamp: at(5),
			LLMResponse: adkmodel.LLMResponse{
				ErrorCode: "RESOURCE_EXHAUSTED", ErrorMessage: "上游限流",
			},
		},
	}
	for _, ev := range want {
		if err := svc.AppendEvent(ctx, created.Session, ev); err != nil {
			t.Fatal(err)
		}
	}

	// What ADK reads back, one session at a time.
	got, err := svc.Get(ctx, &adksession.GetRequest{
		AppName: evApp, UserID: evUser, SessionID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	var viaADK []*adksession.Event
	for ev := range got.Session.Events().All() {
		viaADK = append(viaADK, ev)
	}

	// What the batch read returns.
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batched, err := EventsOf(ctx, db, evApp, evUser, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	viaBatch := batched[id]

	if len(viaBatch) != len(viaADK) {
		t.Fatalf("批量读到 %d 条，ADK 读到 %d 条", len(viaBatch), len(viaADK))
	}
	// Order first, and separately: the projection walks events in order, so
	// the two reads agreeing on content but not on sequence would still show
	// a different timeline.
	for i := range viaADK {
		if viaADK[i].ID != viaBatch[i].ID {
			t.Fatalf("顺序不一致：第 %d 条 ADK 是 %s，批量是 %s",
				i, viaADK[i].ID, viaBatch[i].ID)
		}
	}
	for i := range viaADK {
		a, b := viaADK[i], viaBatch[i]
		if a.ID != b.ID || a.Author != b.Author || a.InvocationID != b.InvocationID {
			t.Errorf("第 %d 条身份不一致: ADK %+v, 批量 %+v", i, a, b)
		}
		if a.Branch != b.Branch {
			t.Errorf("第 %d 条 branch: %q vs %q", i, a.Branch, b.Branch)
		}
		if !a.Timestamp.Equal(b.Timestamp) {
			t.Errorf("第 %d 条时间: %s vs %s", i, a.Timestamp, b.Timestamp)
		}
		if a.ErrorCode != b.ErrorCode || a.ErrorMessage != b.ErrorMessage {
			t.Errorf("第 %d 条错误字段: %q/%q vs %q/%q", i,
				a.ErrorCode, a.ErrorMessage, b.ErrorCode, b.ErrorMessage)
		}
		if a.Actions.TransferToAgent != b.Actions.TransferToAgent {
			t.Errorf("第 %d 条 transfer: %q vs %q", i,
				a.Actions.TransferToAgent, b.Actions.TransferToAgent)
		}
		if (a.Content == nil) != (b.Content == nil) {
			t.Errorf("第 %d 条 content 一边有一边没有", i)
			continue
		}
		if a.Content != nil && !samePartsEnough(a.Content, b.Content) {
			t.Errorf("第 %d 条 content 不一致:\n  ADK  %+v\n  批量 %+v", i, a.Content, b.Content)
		}
		if (a.UsageMetadata == nil) != (b.UsageMetadata == nil) {
			t.Errorf("第 %d 条 usage 一边有一边没有", i)
		} else if a.UsageMetadata != nil &&
			a.UsageMetadata.TotalTokenCount != b.UsageMetadata.TotalTokenCount {
			t.Errorf("第 %d 条 usage: %d vs %d", i,
				a.UsageMetadata.TotalTokenCount, b.UsageMetadata.TotalTokenCount)
		}
	}
}

// samePartsEnough compares the parts of a content the projection reads: text,
// the call and the response. Not a deep equality — genai carries fields
// neither side sets.
func samePartsEnough(a, b *genai.Content) bool {
	if a.Role != b.Role || len(a.Parts) != len(b.Parts) {
		return false
	}
	for i := range a.Parts {
		p, q := a.Parts[i], b.Parts[i]
		if p.Text != q.Text {
			return false
		}
		if (p.FunctionCall == nil) != (q.FunctionCall == nil) {
			return false
		}
		if p.FunctionCall != nil &&
			(p.FunctionCall.ID != q.FunctionCall.ID || p.FunctionCall.Name != q.FunctionCall.Name) {
			return false
		}
		if (p.FunctionResponse == nil) != (q.FunctionResponse == nil) {
			return false
		}
		if p.FunctionResponse != nil &&
			(p.FunctionResponse.ID != q.FunctionResponse.ID ||
				p.FunctionResponse.Name != q.FunctionResponse.Name) {
			return false
		}
	}
	return true
}

// A column that will not decode has to say which one, on which event, in
// which session — and take that session out rather than hand back a task with
// its content missing.
func TestADamagedColumnIsReportedAndItsSessionDropped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	svc, closeSvc, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSvc()

	for _, id := range []string{"web-bad", "web-good"} {
		created, err := svc.Create(ctx, &adksession.CreateRequest{
			AppName: evApp, UserID: evUser, SessionID: id,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{
			ID: id + "-e1", Author: "user", InvocationID: "inv-1",
			Timestamp:   time.Now().UTC(),
			LLMResponse: adkmodel.LLMResponse{Content: genai.NewContentFromText("x", genai.RoleUser)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The shape ADK would write if it changed how it encodes a column, or
	// what a half-written row looks like.
	if _, err := db.Exec(
		`UPDATE events SET content = ? WHERE id = ?`, `{"parts": "not an array"}`, "web-bad-e1",
	); err != nil {
		t.Fatal(err)
	}

	got, err := EventsOf(ctx, db, evApp, evUser, []string{"web-bad", "web-good"})
	if err == nil {
		t.Fatal("坏掉的列被静默吞掉了")
	}

	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("报的不是可定位的解码错误: %v", err)
	}
	for _, want := range []string{"web-bad", "web-bad-e1", "content"} {
		if !strings.Contains(de.Error(), want) {
			t.Errorf("错误里没提到 %q: %s", want, de)
		}
	}

	if _, present := got["web-bad"]; present {
		t.Error("解不开的会话仍然被返回了 —— 它会显示成一个什么都没做的任务")
	}
	if len(got["web-good"]) != 1 {
		t.Errorf("一个坏会话拖累了好的：web-good 有 %d 条事件", len(got["web-good"]))
	}
}
