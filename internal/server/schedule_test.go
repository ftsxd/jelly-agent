package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
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
	prev := reloadStep
	var once sync.Once
	reloadStep = func(step string) {
		if step != "swapped" {
			return
		}
		entered <- struct{}{}
		once.Do(func() { <-release }) // only the first one waits
	}
	t.Cleanup(func() { reloadStep = prev })

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

// The last save on disk is the one that ends up running.
//
// reload read the config file before taking the lock, so two of them could
// each pick up a different version and then apply them in the other order:
// the one holding the older file takes the lock second and installs it,
// silently undoing a save that happened after it. The console shows the newer
// values — they are on disk — while the process runs the older ones, and
// nothing looks wrong until somebody wonders why the new API key is not being
// used.
func TestTheConfigOnDiskIsTheOneThatEndsUpRunning(t *testing.T) {
	s, path := newProviderServer(t)

	held := make(chan struct{})
	release := make(chan struct{})
	prev := reloadStep
	var first atomic.Bool
	reloadStep = func(step string) {
		// Only the first reload waits, and a plain flag rather than sync.Once
		// because Do blocks every later caller until the first returns —
		// which is the second reload, the one this test needs to get past.
		if step == "read" && first.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() { reloadStep = prev })

	slow := make(chan error, 1)
	go func() { slow <- s.reload() }()
	<-held // one reload has read the config and applied nothing

	// Meanwhile the config changes and another reload picks it up.
	raw, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	raw.Providers[0].Model = "写在后面的那个模型"
	if err := config.Save(raw, path); err != nil {
		t.Fatal(err)
	}
	// In a goroutine: with the read inside the lock the paused reload is
	// holding it, so this one waits — and a test that called it inline would
	// deadlock against its own release below.
	fast := make(chan error, 1)
	go func() { fast <- s.reload() }()
	time.Sleep(100 * time.Millisecond)

	close(release)
	for _, err := range []error{<-slow, <-fast} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := s.engine().Config().Providers[0].Model; got != "写在后面的那个模型" {
		t.Errorf("后保存的配置被一次更早开始的 reload 覆盖回去了: model = %q", got)
	}
}
