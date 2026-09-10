package server

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A config save must not kill a scheduled run that is already going.
//
// Every save restarts the cron, and the restart used to cancel the context
// its jobs were given. A nightly inspection eight minutes into its work died
// mid-turn because somebody saved a provider in the console, and the record
// that says what happened was never written — which reads, to whoever looks
// at it, as a schedule that never fired.
func TestSavingConfigDoesNotKillARunningSchedule(t *testing.T) {
	s, _ := newProviderServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.StartSchedules(ctx)
	t.Cleanup(s.stopSchedules)

	s.schedule.mu.Lock()
	jobs := s.schedule.jobs
	s.schedule.mu.Unlock()
	if jobs == nil {
		t.Fatal("排程没起来")
	}

	if err := s.reload(); err != nil { // what saving anything in the console does
		t.Fatal(err)
	}
	if err := jobs.Err(); err != nil {
		t.Fatalf("保存一次配置就把正在跑的排程掐了: %v", err)
	}

	// Shutting the process down still stops them, which is the reason the
	// cancellation existed in the first place.
	cancel()
	if jobs.Err() == nil {
		t.Error("进程退出时排程的上下文没有被取消")
	}
}

// Two config saves at once must not leave a second cron and a second set of
// bots running.
//
// The engine pointer was swapped under a lock, but the bots and the cron were
// replaced after it was released — stop what is running, build the
// replacement, store it. Two of those interleaving each find nothing to stop
// and each store their own, so the first one's cron keeps firing with nobody
// holding it: every scheduled task runs twice, forever, and a restart is the
// only way out. Saving twice quickly in the console is enough to do it.
func TestTwoReloadsAtOnceDoNotLeaveADuplicateRunning(t *testing.T) {
	s, _ := newProviderServer(t)

	entered := make(chan struct{}, 8) // one per reload that got past the swap
	release := make(chan struct{})
	prev := duringReload
	var once sync.Once
	duringReload = func() {
		entered <- struct{}{}
		once.Do(func() { <-release }) // only the first one waits
	}
	t.Cleanup(func() { duringReload = prev })

	done := make(chan error, 2)
	go func() { done <- s.reload() }()
	<-entered // the first reload is between the swap and the restarts

	go func() { done <- s.reload() }()

	// The second one must be waiting to start, not running alongside.
	select {
	case <-entered:
		t.Fatal("两次 reload 同时在替换排程和机器人 —— 先起来的那一套会没人管地继续跑")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	<-entered
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
