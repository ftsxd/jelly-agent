package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"
)

// The in-flight slot must not be freed before the answer it holds is readable.
//
// Freeing it first leaves a window where a server has no shared call and no
// result: a turn arriving inside it finds nothing to join and dials again, for
// an answer that was already sitting there. It is a window of a few
// instructions, which is why it showed up as two or three extra dials in
// twenty turns rather than as an obvious bug — the sort of rate that gets
// blamed on the network.
//
// Checked by watching the two facts together: from the moment the fetch
// returns, "slot gone" must never be observed while "answer unpublished" still
// holds.
func TestTheInFlightSlotOutlivesTheAnswerItHolds(t *testing.T) {
	l := newListing()
	release := make(chan struct{})
	returned := make(chan struct{})
	var calls atomic.Int64

	go func() {
		_, _ = l.do(context.Background(), "n9e", func(context.Context) ([]adktool.Tool, error) {
			calls.Add(1)
			<-release
			close(returned)
			return []adktool.Tool{&stubTool{name: "query_range"}}, nil
		})
	}()

	// Grab the call while the fetch is still blocked, so the channel to watch
	// is in hand before anything can be torn down.
	var c *listingCall
	for i := 0; i < 2000 && c == nil; i++ {
		l.mu.Lock()
		c = l.calls["n9e"]
		l.mu.Unlock()
		if c == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if c == nil {
		t.Fatal("fetch never registered an in-flight call")
	}

	// Hold the map lock, then let the fetch finish. Freeing the slot needs
	// this lock, so the tear-down stops here with whatever it has already
	// done — which is exactly the state a late caller would find.
	l.mu.Lock()
	close(release)
	<-returned
	time.Sleep(50 * time.Millisecond) // long enough to be parked on the lock

	published := false
	select {
	case <-c.done:
		published = true
	default:
	}
	l.mu.Unlock()

	if !published {
		t.Fatal("答案还没发布，释放槽位的动作却已经在等锁：这中间到达的调用会为已有结果重新拨号")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}
}

// The verdict has to be in place before the shared call is published.
//
// Recorded by whichever caller received the answer, it landed after the
// in-flight slot was freed — so a turn arriving in between saw a server with
// no cooldown and no shared call, and dialled it. Recording it on the fetch
// means the answer and the verdict become visible together.
//
// Checked from a waiter: the waiter never writes health itself, so if it can
// see the failure the moment its wait returns, the writing happened on the
// fetch.
func TestTheHealthVerdictIsWrittenByTheFetchNotTheCaller(t *testing.T) {
	blocked := &blockingSet{release: make(chan struct{})}
	health := newToolsetHealth()
	sel := &selectingToolset{
		static:   []adktool.Tool{&stubTool{name: "web_search"}},
		sets:     []namedSet{{name: "n9e-mcp", set: blocked}},
		health:   health,
		inflight: newListing(),
	}

	// A leader that starts the fetch and then abandons it. Its own failure
	// path is never reached, so anything it would have recorded is not
	// recorded at all.
	ctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = sel.Tools(&askingCtx{StrictContextMock: agent.StrictContextMock{Ctx: ctx}})
	}()
	for i := 0; i < 2000 && blocked.calls.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-leaderDone

	close(blocked.release)

	// The fetch finishes with nobody left to record its verdict, and the
	// server must still be known to be down.
	for i := 0; i < 2000; i++ {
		if health.skip("n9e-mcp") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("拨号已经失败，健康状态却没人写入——下一轮会重新拨这台已知不可达的服务器")
}
