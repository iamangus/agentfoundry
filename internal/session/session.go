package session

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

type Message struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Time    time.Time `json:"time"`
}

type Session struct {
	ID          string    `json:"id"`
	AgentName   string    `json:"agent_name"`
	Messages    []Message `json:"messages"`
	CreatedAt   time.Time `json:"created_at"`
	ActiveRunID string    `json:"active_run_id,omitempty"`
	Owner       string    `json:"owner"`
}

type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

func New() *Store {
	return &Store{sessions: make(map[string]*Session)}
}

func (s *Store) Create(agentName string, owner string) *Session {
	id := newID()
	sess := &Session{
		ID:        id,
		AgentName: agentName,
		Messages:  []Message{},
		CreatedAt: time.Now(),
		Owner:     owner,
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess
}

func (s *Store) Get(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Store) ListByOwner(owner string) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if sess.Owner == owner {
			out = append(out, sess)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[j].CreatedAt.Before(out[i].CreatedAt)
	})
	return out
}

func (s *Store) List() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[j].CreatedAt.Before(out[i].CreatedAt)
	})
	return out
}

func (s *Store) AddMessage(id string, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session not found")
	}
	sess.Messages = append(sess.Messages, msg)
	return nil
}

func (s *Store) SetActiveRunID(id, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session not found")
	}
	sess.ActiveRunID = runID
	return nil
}

func (s *Store) ClearActiveRunID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return
	}
	sess.ActiveRunID = ""
}

func (s *Store) FindByRunID(runID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ActiveRunID == runID {
			return sess
		}
	}
	return nil
}

func newID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
