package tool

import (
	"fmt"
	"sync"
)

// The stale-snapshot gate.
//
// Telling the model "先问用户要不要同步" in a tool description does not hold.
// What it did instead was read 140 service directories, spend 112k prompt
// tokens on a snapshot it had already been told was out of date, and put the
// question at the bottom of the answer — where it is worth nothing, because by
// then the analysis it would have changed is already written.
//
// So the first time a conversation learns that a project's snapshot is behind
// the remote, the code-reading tools refuse for the rest of that turn. The
// model has nothing left to do but answer, and the only useful answer is the
// question. Whatever the person says next arrives as a new turn, and the gate
// is already spent: "不用同步，就看现在的代码" works on the very next message,
// and a conversation is never asked twice about the same project.
//
// Deliberately not a policy about staleness in general — a week-old snapshot
// with no new commits is not stale, and this never fires for it.
type staleGate struct {
	mu    sync.Mutex
	armed map[string]armedStale
	order []string
}

type armedStale struct {
	invocation string
	local      string
	remote     string
}

// maxGateSessions bounds the bookkeeping, like the tool admissions map. Losing
// an entry costs one conversation an extra question, which is the mild side of
// this trade.
const maxGateSessions = 512

var gate = &staleGate{armed: map[string]armedStale{}}

func gateKey(session, project string) string { return session + "\x00" + project }

// arm records that this conversation has just been told the project is behind.
// It fires once per conversation and project: the second listing in the same
// session must not re-arm, or answering "不用同步" would be met with the same
// question on the next turn, forever.
func (g *staleGate) arm(session, invocation, project, local, remote string) {
	if session == "" || invocation == "" {
		return
	}
	key := gateKey(session, project)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.armed[key]; ok {
		return
	}
	g.armed[key] = armedStale{invocation: invocation, local: local, remote: remote}
	g.order = append(g.order, key)
	if len(g.order) > maxGateSessions {
		delete(g.armed, g.order[0])
		g.order = g.order[1:]
	}
}

// check reports the refusal this turn owes the person, or nil. Only the turn
// that was told holds; a later turn carries the person's answer and proceeds.
func (g *staleGate) check(session, invocation, project string) error {
	if session == "" || invocation == "" {
		return nil
	}
	g.mu.Lock()
	a, ok := g.armed[gateKey(session, project)]
	g.mu.Unlock()
	if !ok || a.invocation != invocation {
		return nil
	}
	return fmt.Errorf("先确认再分析：%s 的代码快照落后于远端（本地 %s，远端 %s）。"+
		"请现在就回答用户这一件事——本地和远端各是什么版本、不同步会漏掉什么、要不要现在同步——"+
		"不要在本轮继续读代码。用户回答「同步」就调用 sync_project；回答「不用」或问别的，下一轮即可照常分析",
		project, short(a.local), short(a.remote))
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	if rev == "" {
		return "无"
	}
	return rev
}
