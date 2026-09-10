package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// Every handler has to reach the engine through the request, not through
// whatever is current.
//
// The pin only guarantees the engine a request *started* on. A handler that
// calls s.engine() gets the one that is current at that moment, which after a
// config save is a different engine — and after a second one, an engine that
// may already have closed its database handles while this request is still
// using them. The middleware cannot prevent that on its own; it only makes
// the right engine available, and this is what makes handlers take it.
//
// s.engine().Config() is allowed: a *config.Config is plain data that outlives
// the engine it came from, so reading the newest one mid-request is safe.
func TestHandlersReachTheEngineThroughTheRequest(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("*.go"))
	if err != nil {
		t.Fatal(err)
	}
	// s.engine() followed by anything other than .Config().
	live := regexp.MustCompile(`s\.engine\(\)(?:\.Config\(\))?`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "enginepin.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, m := range live.FindAllString(line, -1) {
				if m == "s.engine()" {
					t.Errorf("%s:%d 用 s.engine() 取了引擎持有的东西 —— "+
						"请求里要用 s.engineFor(r)，后台任务用 s.pin()：\n\t%s",
						f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

// Two config saves during one request, which is what the middleware alone
// does not survive.
//
// The handler here takes its database handle the way every handler does —
// s.stateDB(w, r) — but only after the first save. If that resolves to
// "whatever is current" it hands back the second engine, which nothing has
// pinned; the second save then retires it with no users and closes it, and
// the handle dies in the middle of a request that is still running.
func TestAHandlerKeepsItsDatabaseAcrossTwoConfigSaves(t *testing.T) {
	s, _ := newProviderServer(t)
	s.ref.eng.SetStateRef(filepath.Join(t.TempDir(), "state.db"))

	started := make(chan struct{})
	firstSave := make(chan struct{})
	secondSave := make(chan struct{})
	done := make(chan error, 1)

	h := s.pinEngine(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-firstSave
		db, ok := s.stateDB(w, r) // the ordinary way a handler gets one
		if !ok {
			done <- errors.New("stateDB 打不开")
			return
		}
		<-secondSave
		var one int
		done <- db.QueryRow("SELECT 1").Scan(&one)
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/health", nil))

	<-started
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	close(firstSave)
	// Give the handler time to take its handle before the second save.
	time.Sleep(100 * time.Millisecond)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	close(secondSave)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("请求还在跑，它的状态库就被关了: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("请求没结束")
	}
}
