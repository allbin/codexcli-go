package codexcli

import (
	"context"
	"sync"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// Codex subagents are full threads on the same connection. They never send
// thread/started; a parent learns of one from a subAgentActivity item in
// its own turn, and the child's first frame (thread/status/changed) can
// arrive just before that item. Observed on codex 0.148 and 0.159.3.

// ChildThread is a subagent thread this connection has seen spawned.
type ChildThread struct {
	// ThreadID is the child's thread id.
	ThreadID string
	// ParentThreadID is the thread that spawned it: a thread from
	// NewThread or ResumeThread, or another child.
	ParentThreadID string
	// AgentPath is codex's hierarchical name for it, such as "/root/alpha".
	AgentPath string
	// ToolCallID is the id of the parent's tool call that spawned it, the
	// subAgentActivity item id. Empty if only a later activity was seen.
	ToolCallID string
	// SpawnTurnID is the parent turn the child was spawned in.
	SpawnTurnID string
	// ActiveTurnID is the child's running turn, or "" when it is idle.
	ActiveTurnID string
}

// ChildThreadEvent carries an event from a subagent thread on the Stream of
// the thread at the top of its tree, the one created with NewThread or
// ResumeThread. Event is the child's event as it would appear on its own
// Stream, with the child's thread id; Child identifies the child as of
// that event.
//
// A child's TurnCompletedEvent ends the child's turn, not the Stream's.
// Child events reach the root's current Stream only: while no Stream is
// open on the root thread they are dropped, like the root's own events.
// Approvals a child asks for arrive here as an ApprovalRequestEvent and go
// to the approval handler as usual, with the child's ThreadID.
type ChildThreadEvent struct {
	Child ChildThread
	Event Event
}

func (*ChildThreadEvent) event() {}

const (
	// childInterruptTimeout bounds each subagent's turn/interrupt in a
	// cascading Thread.Interrupt, so a wedged child cannot hold up the
	// parent's.
	childInterruptTimeout = 3 * time.Second
	// orphanThreadLimit and orphanEventLimit bound the frames held for
	// threads not yet named by a subAgentActivity.
	orphanThreadLimit = 32
	orphanEventLimit  = 256
	// childLimit bounds the registry; past it the oldest idle children
	// are forgotten.
	childLimit = 1024
)

// heldEvent is an event for an unnamed thread and that thread's active
// turn when it arrived.
type heldEvent struct {
	ev   Event
	turn string
}

// childRegistry tracks subagent threads and holds frames for threads no
// subAgentActivity has named yet. Guarded by its own mutex; Conn.subsMu,
// when both are needed, is taken first.
type childRegistry struct {
	mu       sync.Mutex
	children map[string]*ChildThread
	// childOrder is children's ids, oldest first, for eviction.
	childOrder []string
	// turns is the active turn of every thread not registered with
	// NewThread/ResumeThread, named or not, so a cascade can interrupt a
	// child whose turn/started arrived before it was named.
	turns map[string]string
	// orphans holds events for unnamed threads, oldest thread first in
	// orphanOrder.
	orphans     map[string][]heldEvent
	orphanOrder []string
}

func (r *childRegistry) init() {
	if r.children == nil {
		r.children = map[string]*ChildThread{}
		r.turns = map[string]string{}
		r.orphans = map[string][]heldEvent{}
	}
}

// snapshot returns the child with its current active turn. r.mu held.
func (r *childRegistry) snapshot(c *ChildThread) ChildThread {
	out := *c
	out.ActiveTurnID = r.turns[c.ThreadID]
	return out
}

// forget drops a thread from the tree, as a child and as an orphan.
// r.mu held.
func (r *childRegistry) forget(threadID string) {
	if _, ok := r.children[threadID]; ok {
		delete(r.children, threadID)
		r.childOrder = removeID(r.childOrder, threadID)
	}
	if _, ok := r.orphans[threadID]; ok {
		delete(r.orphans, threadID)
		r.orphanOrder = removeID(r.orphanOrder, threadID)
	}
	delete(r.turns, threadID)
}

// add registers a child, evicting the oldest idle children past
// childLimit. r.mu held.
func (r *childRegistry) add(c *ChildThread) {
	r.children[c.ThreadID] = c
	r.childOrder = append(r.childOrder, c.ThreadID)
	for i := 0; len(r.children) > childLimit && i < len(r.childOrder); {
		id := r.childOrder[i]
		if r.turns[id] != "" {
			i++
			continue
		}
		delete(r.children, id)
		r.childOrder = append(r.childOrder[:i], r.childOrder[i+1:]...)
	}
}

func removeID(ids []string, id string) []string {
	for i, x := range ids {
		if x == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

// root walks up from a child to the first ancestor that is not a child.
// r.mu held.
func (r *childRegistry) root(c *ChildThread) string {
	id := c.ParentThreadID
	for range 64 {
		p, ok := r.children[id]
		if !ok {
			return id
		}
		id = p.ParentThreadID
	}
	return id
}

// routeUnsubscribed handles an event for a thread with no subscriber. A
// child's event goes to its root's subscriber; an unnamed foreign thread's
// is held until a subAgentActivity names it. subsMu held.
func (c *Conn) routeUnsubscribed(threadID string, ev Event) {
	if threadID == "" || c.lookupThread(threadID) != nil {
		return
	}
	r := &c.childReg
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	if child, ok := r.children[threadID]; ok {
		if sub := c.subs[r.root(child)]; sub != nil {
			sub.push(&ChildThreadEvent{Child: r.snapshot(child), Event: ev})
		}
		return
	}
	held, ok := r.orphans[threadID]
	if !ok {
		if len(r.orphanOrder) >= orphanThreadLimit {
			r.forget(r.orphanOrder[0])
		}
		r.orphanOrder = append(r.orphanOrder, threadID)
	}
	if len(held) >= orphanEventLimit {
		held = held[1:]
	}
	r.orphans[threadID] = append(held, heldEvent{ev: ev, turn: r.turns[threadID]})
}

// noteForeignTurn records turn starts and ends on threads this connection
// did not create, so a cascade knows each child's running turn.
func (c *Conn) noteForeignTurn(threadID, turnID string, started bool) {
	if c.lookupThread(threadID) != nil {
		return
	}
	r := &c.childReg
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	switch {
	case started:
		r.turns[threadID] = turnID
	case r.turns[threadID] == turnID:
		delete(r.turns, threadID)
	}
}

// noteSubAgent registers the child a subAgentActivity item names and
// replays anything held for it.
func (c *Conn) noteSubAgent(parentThreadID, parentTurnID string, item *schema.ThreadItem) {
	a := item.SubAgentActivity()
	if a == nil || a.AgentThreadID == "" || a.AgentThreadID == parentThreadID || c.lookupThread(a.AgentThreadID) != nil {
		return
	}
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	r := &c.childReg
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	child, known := r.children[a.AgentThreadID]
	if !known {
		child = &ChildThread{ThreadID: a.AgentThreadID, ParentThreadID: parentThreadID, SpawnTurnID: parentTurnID}
		r.add(child)
	}
	if a.AgentPath != "" {
		child.AgentPath = a.AgentPath
	}
	if a.Kind == schema.SubAgentActivityStarted && child.ToolCallID == "" {
		child.ToolCallID = item.ID
	}
	held := r.orphans[a.AgentThreadID]
	if len(held) == 0 {
		return
	}
	delete(r.orphans, a.AgentThreadID)
	r.orphanOrder = removeID(r.orphanOrder, a.AgentThreadID)
	if sub := c.subs[r.root(child)]; sub != nil {
		for _, h := range held {
			snap := *child
			snap.ActiveTurnID = h.turn
			sub.push(&ChildThreadEvent{Child: snap, Event: h.ev})
		}
	}
}

// detachThread makes a thread a root: called when NewThread or
// ResumeThread registers it, so a resumed former child gets its own
// subagents' events instead of routing them to its old parent.
func (c *Conn) detachThread(threadID string) {
	r := &c.childReg
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	r.forget(threadID)
}

// forgetDeleted drops a deleted thread, and the subagents codex deleted
// with it, from the connection's bookkeeping.
func (c *Conn) forgetDeleted(threadID string) {
	gone := c.descendants(threadID)
	c.threadsMu.Lock()
	delete(c.threads, threadID)
	c.threadsMu.Unlock()
	r := &c.childReg
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	r.forget(threadID)
	for _, child := range gone {
		r.forget(child.ThreadID)
	}
}

// ChildThread reports a subagent thread this connection has seen spawned.
func (c *Conn) ChildThread(threadID string) (ChildThread, bool) {
	r := &c.childReg
	r.mu.Lock()
	defer r.mu.Unlock()
	if child, ok := r.children[threadID]; ok {
		return r.snapshot(child), true
	}
	return ChildThread{}, false
}

// descendants returns every child under threadID, deepest first.
func (c *Conn) descendants(threadID string) []ChildThread {
	r := &c.childReg
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ChildThread
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		if depth > 64 {
			return
		}
		for _, child := range r.children {
			if child.ParentThreadID == parent {
				walk(child.ThreadID, depth+1)
				out = append(out, r.snapshot(child))
			}
		}
	}
	walk(threadID, 0)
	return out
}

// Children returns every subagent thread spawned under t on this
// connection, at any depth, deepest first. A child appears once a
// subAgentActivity has named it; ActiveTurnID says whether it is working.
func (t *Thread) Children() []ChildThread { return t.conn.descendants(t.ID) }

// interruptDescendants interrupts every running descendant turn of
// threadID, concurrently, each bounded by childInterruptTimeout. Failures
// are logged, not returned: the parent's interrupt must still go out.
func (c *Conn) interruptDescendants(ctx context.Context, threadID string) {
	var wg sync.WaitGroup
	for _, child := range c.descendants(threadID) {
		if child.ActiveTurnID == "" {
			continue
		}
		wg.Add(1)
		go func(child ChildThread) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, childInterruptTimeout)
			defer cancel()
			if err := c.interruptBounded(cctx, child.ThreadID, child.ActiveTurnID); err != nil {
				c.logger.Debug("codexcli: subagent interrupt failed", "thread", child.ThreadID, "err", err)
			}
		}(child)
	}
	wg.Wait()
}

// interruptBounded is interrupt that returns at ctx's deadline even when
// the request write itself blocks.
func (c *Conn) interruptBounded(ctx context.Context, threadID, turnID string) error {
	done := make(chan error, 1)
	go func() { done <- c.interrupt(ctx, threadID, turnID) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
