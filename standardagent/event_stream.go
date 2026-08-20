package standardagent

import (
	"context"
	"sync"

	"github.com/LyleLiu666/agentSlot/interaction"
)

const eventStreamBufferLimit = 1024

// eventHub is per Session Runtime. It publishes temporary stream output and
// durable commit notifications without persisting client cursors or chunks.
type eventHub struct {
	mu          sync.Mutex
	subscribers map[*runtimeEventStream]struct{}
	closed      bool
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: make(map[*runtimeEventStream]struct{})}
}

func (h *eventHub) subscribe() (interaction.EventStream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, interaction.ErrEventStreamClosed
	}
	stream := &runtimeEventStream{hub: h, changed: make(chan struct{})}
	h.subscribers[stream] = struct{}{}
	return stream, nil
}

func (h *eventHub) publish(event interaction.Event) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for stream := range h.subscribers {
		if !stream.enqueue(cloneInteractionEvent(event)) {
			delete(h.subscribers, stream)
		}
	}
}

func (h *eventHub) remove(stream *runtimeEventStream) {
	h.mu.Lock()
	delete(h.subscribers, stream)
	h.mu.Unlock()
}

func (h *eventHub) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	streams := make([]*runtimeEventStream, 0, len(h.subscribers))
	for stream := range h.subscribers {
		streams = append(streams, stream)
	}
	h.subscribers = nil
	h.mu.Unlock()
	for _, stream := range streams {
		stream.closeFromHub()
	}
}

type runtimeEventStream struct {
	hub     *eventHub
	mu      sync.Mutex
	changed chan struct{}
	queue   []interaction.Event
	closed  bool
	err     error
}

func (s *runtimeEventStream) Recv(ctx context.Context) (interaction.Event, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			event := s.queue[0]
			s.queue[0] = interaction.Event{}
			if len(s.queue) == 1 {
				s.queue = nil
			} else {
				s.queue = s.queue[1:]
			}
			s.mu.Unlock()
			return event, nil
		}
		if s.closed {
			err := s.err
			s.mu.Unlock()
			if err != nil {
				return interaction.Event{}, err
			}
			return interaction.Event{}, interaction.ErrEventStreamClosed
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return interaction.Event{}, ctx.Err()
		}
	}
}

func (s *runtimeEventStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.queue = nil
	close(s.changed)
	s.mu.Unlock()
	s.hub.remove(s)
	return nil
}

func (s *runtimeEventStream) enqueue(event interaction.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if len(s.queue) >= eventStreamBufferLimit {
		if event.Kind == interaction.EventChunk || event.Kind == interaction.EventReset {
			return true
		}
		dropped := false
		for index, queued := range s.queue {
			if queued.Kind != interaction.EventChunk && queued.Kind != interaction.EventReset {
				continue
			}
			copy(s.queue[index:], s.queue[index+1:])
			s.queue[len(s.queue)-1] = interaction.Event{}
			s.queue = s.queue[:len(s.queue)-1]
			dropped = true
			break
		}
		if !dropped {
			s.closed = true
			s.err = interaction.ErrEventStreamOverflow
			s.queue = nil
			close(s.changed)
			return false
		}
	}
	s.queue = append(s.queue, event)
	close(s.changed)
	s.changed = make(chan struct{})
	return true
}

func (s *runtimeEventStream) closeFromHub() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.queue = nil
	close(s.changed)
	s.mu.Unlock()
}

func cloneInteractionEvent(source interaction.Event) interaction.Event {
	return source
}
