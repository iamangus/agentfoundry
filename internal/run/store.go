package run

import (
	"fmt"
	"sync"
	"time"
)

type Status string

const (
	StatusRunning  Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

type Run struct {
	ID         string    `json:"id"`
	AgentName  string    `json:"agent"`
	Status     Status    `json:"status"`
	Response   string    `json:"response,omitempty"`
	Error      string    `json:"error,omitempty"`
	WorkflowID string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

type Store struct {
	mu   sync.Mutex
	runs map[string]*Run
}

func New() *Store {
	return &Store{runs: make(map[string]*Run)}
}

func (s *Store) Create(agentName string) *Run {
	r := &Run{
		ID:        newID(),
		AgentName: agentName,
		Status:    StatusRunning,
		CreatedAt: time.Now(),
	}
	s.mu.Lock()
	s.runs[r.ID] = r
	s.mu.Unlock()
	return r
}

func (s *Store) Get(id string) (*Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	return r, ok
}

func (s *Store) UpdateStatus(id string, status Status, response, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("run %s not found", id)
	}
	r.Status = status
	r.Response = response
	r.Error = errMsg
	return nil
}

func (s *Store) SetWorkflowID(id, workflowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("run %s not found", id)
	}
	r.WorkflowID = workflowID
	return nil
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	delete(s.runs, id)
	s.mu.Unlock()
}

func newID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
