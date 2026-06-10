package stream

import "sync"

const subBufferSize = 64

type Event struct {
	ID   int64
	Type string
	Data string
}

type Stream struct {
	mu     sync.Mutex
	events []Event
	subs   []chan Event
	closed bool
	ready  chan struct{}
	nextID int64
}

func (s *Stream) publish(evt Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evt.ID = s.nextID
	s.nextID++
	s.events = append(s.events, evt)
	for _, ch := range s.subs {
		select {
		case ch <- evt:
		default:
		}
	}
	if evt.Type == "done" || evt.Type == "error" {
		s.closed = true
		for _, ch := range s.subs {
			close(ch)
		}
		s.subs = nil
	}
}

func (s *Stream) Subscribe() (<-chan Event, func()) {
	return s.subscribeFrom(0)
}

func (s *Stream) subscribeFrom(fromID int64) (<-chan Event, func()) {
	ch := make(chan Event, subBufferSize)
	s.mu.Lock()
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
	ready := s.ready
	for _, evt := range s.events {
		if evt.ID > fromID {
			ch <- evt
		}
	}
	if s.closed {
		close(ch)
		s.mu.Unlock()
		return ch, func() {}
	}
	s.subs = append(s.subs, ch)
	s.mu.Unlock()

	first := true
	unsubscribe := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, sub := range s.subs {
			if sub == ch {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				break
			}
		}
		if first {
			first = false
			close(ready)
		}
	}
	return ch, unsubscribe
}

func (s *Stream) SubscribeFrom(eventID int64) (<-chan Event, func()) {
	return s.subscribeFrom(eventID)
}

func (s *Stream) LatestID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextID - 1
}

func (s *Stream) Ready() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
	return s.ready
}

type Manager struct {
	mu   sync.Mutex
	runs map[string]*Stream
}

func NewManager() *Manager {
	return &Manager{runs: make(map[string]*Stream)}
}

func (m *Manager) Create(id string) *Stream {
	s := &Stream{ready: make(chan struct{})}
	m.mu.Lock()
	m.runs[id] = s
	m.mu.Unlock()
	return s
}

func (m *Manager) Get(id string) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id]
}

func (m *Manager) Delete(id string) {
	m.mu.Lock()
	delete(m.runs, id)
	m.mu.Unlock()
}

func (m *Manager) PublishToken(id, token string) {
	if s := m.Get(id); s != nil {
		s.publish(Event{Type: "token", Data: token})
	}
}

func (m *Manager) PublishStatus(id, status string) {
	if s := m.Get(id); s != nil {
		s.publish(Event{Type: "status", Data: status})
	}
}

func (m *Manager) PublishDone(id, html string) {
	if s := m.Get(id); s != nil {
		s.publish(Event{Type: "done", Data: html})
	}
}

func (m *Manager) PublishError(id, html string) {
	if s := m.Get(id); s != nil {
		s.publish(Event{Type: "error", Data: html})
	}
}

func (m *Manager) PublishEvent(id, eventType, data string) {
	if s := m.Get(id); s != nil {
		s.publish(Event{Type: eventType, Data: data})
	}
}