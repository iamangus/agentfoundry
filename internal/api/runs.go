package api

import (
	"context"
	"sync"
	"time"
)

// RunStatus represents the lifecycle state of an async agent run.
type RunStatus string

const (
	RunStatusQueued    RunStatus = "queued"
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCanceled  RunStatus = "canceled"
)

// RunInfo holds the state of a single async agent run.
type RunInfo struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Status    RunStatus `json:"status"`
	Response  string    `json:"response,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	cancel context.CancelFunc // unexported; not serialized
}

// RunManager is a thread-safe in-memory store for active and recently
// completed agent runs. Runs are never persisted to disk; if the process
// restarts all run state is lost (the orchestrator is responsible for recovery).
type RunManager struct {
	mu   sync.RWMutex
	runs map[string]*RunInfo
}

// NewRunManager creates an empty RunManager.
func NewRunManager() *RunManager {
	return &RunManager{
		runs: make(map[string]*RunInfo),
	}
}

// Create registers a new run and returns its initial state.
// The cancel function is stored internally and is invoked by Cancel.
func (m *RunManager) Create(id, agent string, cancel context.CancelFunc) *RunInfo {
	now := time.Now()
	info := &RunInfo{
		ID:        id,
		Agent:     agent,
		Status:    RunStatusQueued,
		CreatedAt: now,
		UpdatedAt: now,
		cancel:    cancel,
	}
	m.mu.Lock()
	m.runs[id] = info
	m.mu.Unlock()
	return info
}

// SetRunning transitions a run to the running state.
func (m *RunManager) SetRunning(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[id]; ok {
		r.Status = RunStatusRunning
		r.UpdatedAt = time.Now()
	}
}

// SetCompleted stores the final response and marks the run as completed.
func (m *RunManager) SetCompleted(id, response string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[id]; ok {
		r.Status = RunStatusCompleted
		r.Response = response
		r.UpdatedAt = time.Now()
	}
}

// SetFailed stores the error message and marks the run as failed.
func (m *RunManager) SetFailed(id, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[id]; ok {
		r.Status = RunStatusFailed
		r.Error = errMsg
		r.UpdatedAt = time.Now()
	}
}

// Cancel invokes the run's context cancel function and marks it as canceled.
// Returns false if the run does not exist or is already in a terminal state.
func (m *RunManager) Cancel(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return false
	}
	switch r.Status {
	case RunStatusCompleted, RunStatusFailed, RunStatusCanceled:
		return false
	}
	if r.cancel != nil {
		r.cancel()
	}
	r.Status = RunStatusCanceled
	r.UpdatedAt = time.Now()
	return true
}

// Get returns a copy of the RunInfo for the given id.
// Returns nil if not found.
func (m *RunManager) Get(id string) *RunInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return nil
	}
	// Return a shallow copy so callers cannot mutate the stored state.
	copy := *r
	return &copy
}
