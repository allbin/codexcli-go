package codexcli

import "sync"

// subscription is one Stream's feed of thread events. The read loop pushes
// into an unbounded FIFO and never blocks; a pump goroutine hands events
// to out one at a time, so nothing is dropped however far the consumer
// falls behind. The cost is memory: a consumer that stops reading without
// closing its Stream lets the queue grow for as long as codex keeps
// sending.
type subscription struct {
	out chan Event

	mu      sync.Mutex
	queue   []Event
	closing bool // no more pushes; out closes once the queue drains

	wake      chan struct{} // cap 1: queue or closing changed
	abort     chan struct{} // reader gone: drop the queue, close out now
	abortOnce sync.Once
	pumpDone  chan struct{} // closed when the pump goroutine exits
}

func newSubscription() *subscription {
	s := &subscription{
		out:      make(chan Event),
		wake:     make(chan struct{}, 1),
		abort:    make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
	go s.pump()
	return s
}

// push queues ev unless the subscription is closing. Never blocks.
func (s *subscription) push(ev Event) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, ev)
	s.mu.Unlock()
	s.signal()
}

// finish stops pushes; out closes after everything queued is delivered.
func (s *subscription) finish() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.signal()
}

// cancel is for the reader leaving: queued events are dropped and out
// closes without waiting for anyone to read them.
func (s *subscription) cancel() {
	s.finish()
	s.abortOnce.Do(func() { close(s.abort) })
}

func (s *subscription) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscription) pump() {
	defer close(s.pumpDone)
	defer close(s.out)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return
			}
			select {
			case <-s.wake:
				continue
			case <-s.abort:
				return
			}
		}
		ev := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		if len(s.queue) == 0 {
			s.queue = nil
		}
		s.mu.Unlock()
		select {
		case s.out <- ev:
		case <-s.abort:
			return
		}
	}
}
