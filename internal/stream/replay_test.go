package stream

import "testing"

func TestStreamBoundsReplayAndDeliversTerminalToSlowSubscriber(t *testing.T) {
	s := &Stream{}
	sub, cancel := s.Subscribe()
	defer cancel()
	for i := 0; i < maxReplayEvents+subBufferSize+20; i++ {
		s.publish(Event{Type: "token", Data: "x"})
	}
	s.publish(Event{Type: "done", Data: "final answer"})
	if len(s.events) != maxReplayEvents {
		t.Fatalf("replay retained %d events", len(s.events))
	}
	var last Event
	for evt := range sub {
		last = evt
	}
	if last.Type != "done" || last.Data != "final answer" {
		t.Fatalf("terminal event lost: %+v", last)
	}
	replay, _ := s.Subscribe()
	count := 0
	for range replay {
		count++
	}
	if count != maxReplayEvents {
		t.Fatalf("replayed %d events", count)
	}
}
