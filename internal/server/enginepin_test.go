package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// A config save replaces the engine. The one a request is already running on
// has to stay open until that request is done — the engine owns the state
// database handle, so closing it early is a use-after-close in the middle of
// somebody's page, not a clean error.
func TestSavingConfigDoesNotCloseAnEngineARequestIsStillUsing(t *testing.T) {
	s, _ := newProviderServer(t)
	s.ref.eng.SetStateRef(filepath.Join(t.TempDir(), "state.db"))

	opened := make(chan struct{})   // the request has its handle
	reloaded := make(chan struct{}) // …and the config has been saved since
	done := make(chan error, 1)

	h := s.pinEngine(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		db, err := s.engineFor(r).StateDB()
		if err != nil {
			done <- err
			return
		}
		close(opened)
		<-reloaded
		var one int
		done <- db.QueryRow("SELECT 1").Scan(&one)
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/health", nil))

	<-opened
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	close(reloaded)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("请求还在跑，它的状态库就被关了: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("请求没结束")
	}
}

// …and once it is done, the replaced engine really does get closed. Otherwise
// every config save would leak a connection pool and a set of MCP
// subprocesses, which is the leak the immediate Close was there to avoid.
func TestTheReplacedEngineIsClosedOnceTheRequestFinishes(t *testing.T) {
	s, _ := newProviderServer(t)
	s.ref.eng.SetStateRef(filepath.Join(t.TempDir(), "state.db"))

	old := s.ref.eng
	db, err := old.StateDB()
	if err != nil {
		t.Fatal(err)
	}

	opened := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	h := s.pinEngine(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(opened)
		<-release
	}))
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/health", nil))
		close(returned)
	}()

	<-opened
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("退役的引擎在请求跑完前就关了: %v", err)
	}

	close(release)
	<-returned
	// The close happens on the releasing goroutine, which is this one's peer.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
			return // closed, as it should be
		}
		if time.Now().After(deadline) {
			t.Fatal("请求跑完了，被替换的引擎还没关 —— 每次保存配置都漏一套连接池和 MCP 子进程")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
