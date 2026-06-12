package codexcli

import (
	"strings"
	"sync"

	"github.com/allbin/codexcli-go/schema"
)

// cmdOutputAccumulator buffers streamed command_output deltas keyed by
// item id. Codex app-server delivers a commandExecution item's output
// only through item/commandExecution/outputDelta notifications and in
// practice leaves aggregatedOutput null on the completed item, so a
// consumer that wants the final output otherwise has to reconstruct it
// from the delta stream. When WithAccumulatedOutput is enabled the Conn
// does that reconstruction via applyAccumulatedOutput.
//
// All methods are safe for concurrent use. Notifications are dispatched
// serially by the rpc read loop today, but the mutex keeps the type
// correct under -race and robust to a future concurrent dispatcher.
type cmdOutputAccumulator struct {
	mu  sync.Mutex
	buf map[string]*strings.Builder
}

// append adds a delta to the per-item buffer, allocating lazily.
func (a *cmdOutputAccumulator) append(itemID, delta string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.buf == nil {
		a.buf = make(map[string]*strings.Builder)
	}
	b := a.buf[itemID]
	if b == nil {
		b = &strings.Builder{}
		a.buf[itemID] = b
	}
	b.WriteString(delta)
}

// take returns the accumulated output for itemID and removes the entry,
// draining the buffer so it can't leak across a long-lived connection.
// The boolean is false when nothing was buffered for the item.
func (a *cmdOutputAccumulator) take(itemID string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.buf[itemID]
	if !ok {
		return "", false
	}
	delete(a.buf, itemID)
	return b.String(), true
}

// applyAccumulatedOutput populates a completed commandExecution item's
// AggregatedOutput from the per-item delta buffer when the server left
// it empty, then drains the buffer. A non-empty server value always
// wins. The buffer is drained for any completed item that had one (even
// the non-commandExecution case, which shouldn't happen) so nothing
// leaks.
func (c *Conn) applyAccumulatedOutput(item *schema.ThreadItem) {
	buffered, ok := c.cmdOutput.take(item.ID)
	if !ok {
		return
	}
	if item.Type != schema.ItemTypeCommandExecution {
		return
	}
	if item.AggregatedOutput != nil && *item.AggregatedOutput != "" {
		return
	}
	item.AggregatedOutput = &buffered
}
