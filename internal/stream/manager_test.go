package stream

import (
	"testing"
	"time"
)

func TestSubscribeReplaysBeyondBuffer(t *testing.T) {
	s := &Stream{}
	for i := 0; i < subBufferSize*2; i++ {
		s.publish(Event{Type: "token", Data: "t"})
	}

	done := make(chan struct{})
	go func() {
		ch, unsub := s.Subscribe()
		unsub()
		if n := len(ch); n < subBufferSize*2 {
			t.Errorf("expected full replay of %d events, got %d buffered", subBufferSize*2, n)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe() deadlocked on stream with more than subBufferSize events")
	}
}

func TestSubscribeIncludesFirstEvent(t *testing.T) {
	s := &Stream{}
	s.publish(Event{Type: "status", Data: "start"})
	s.publish(Event{Type: "token", Data: "t1"})

	ch, unsub := s.Subscribe()
	defer unsub()

	if evt, ok := <-ch; !ok || evt.ID != 0 || evt.Data != "start" {
		t.Fatalf("expected first event (ID 0) replayed, got %+v ok=%v", evt, ok)
	}
}

func TestSubscribeFromResumesAfterID(t *testing.T) {
	s := &Stream{}
	for i := 0; i < 5; i++ {
		s.publish(Event{Type: "token", Data: "t"})
	}

	ch, unsub := s.SubscribeFrom(3)
	defer unsub()

	if evt, ok := <-ch; !ok || evt.ID != 4 {
		t.Fatalf("expected event 4 after resuming from 3, got %+v ok=%v", evt, ok)
	}
	if n := len(ch); n != 0 {
		t.Fatalf("expected exactly one replayed event, got %d", n)
	}
}

func TestSubscribeReceivesLiveEvents(t *testing.T) {
	s := &Stream{}
	ch, unsub := s.Subscribe()
	defer unsub()

	s.publish(Event{Type: "token", Data: "live"})

	select {
	case evt := <-ch:
		if evt.Data != "live" {
			t.Fatalf("expected live event, got %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive live event")
	}
}
