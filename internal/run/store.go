package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

var ErrTerminal = errors.New("run is terminal")

const (
	StatusRunning   Status = "running"
	StatusWaiting   Status = "waiting"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

type Run struct {
	ID             string          `json:"id"`
	AgentName      string          `json:"agent"`
	Status         Status          `json:"status"`
	Response       string          `json:"response,omitempty"`
	Error          string          `json:"error,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	WorkflowID     string          `json:"workflow_id,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	Owner          string          `json:"-"`
	SessionID      string          `json:"-"`
	Persistent     bool            `json:"persistent,omitempty"`
	EphemeralNames []string        `json:"-"`
	MCPServers     json.RawMessage `json:"-"`
	CurrentInputID string          `json:"current_input_id,omitempty"`
	LastInputID    string          `json:"last_input_id,omitempty"`
	ClientKey      string          `json:"client_key,omitempty"`
	DispatchKey    string          `json:"-"`
}

// CreatePersistent creates a long-lived run which accepts input signals.
func (s *Store) CreatePersistent(agentName, owner string) *Run {
	r := s.Create(agentName, owner, "", "")
	s.mu.Lock()
	r.Persistent = true
	r.Status = StatusWaiting
	s.mu.Unlock()
	return r
}

func (s *Store) CreatePersistentChecked(agentName, owner string) (*Run, error) {
	r, _, err := s.CreatePersistentWithKeyChecked(agentName, owner, "")
	return r, err
}

func (s *Store) FindActiveClientKey(owner, key string) (*Run, bool) {
	if key == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.Owner == owner && r.ClientKey == key && r.Persistent && (r.Status == StatusWaiting || r.Status == StatusRunning) {
			copy := *r
			return &copy, true
		}
	}
	return nil, false
}

func (s *Store) CreatePersistentWithKeyChecked(agentName, owner, key string, servers ...json.RawMessage) (*Run, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		for _, existing := range s.runs {
			if existing.Owner == owner && existing.ClientKey == key && existing.Persistent && (existing.Status == StatusWaiting || existing.Status == StatusRunning) {
				copy := *existing
				return &copy, false, nil
			}
		}
	}
	r := &Run{ID: newID(), AgentName: agentName, Owner: owner, ClientKey: key, Status: StatusWaiting, Persistent: true, CreatedAt: time.Now()}
	if len(servers) > 0 {
		r.MCPServers = append(json.RawMessage(nil), servers[0]...)
	}
	if err := s.persist(r); err != nil {
		if isUniqueViolation(err) && key != "" {
			if existing, lookupErr := s.loadConflict(owner, "client_key", key); lookupErr == nil {
				s.runs[existing.ID] = existing
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	s.runs[r.ID] = r
	return r, true, nil
}

type Store struct {
	mu     sync.Mutex
	runs   map[string]*Run
	db     *pgxpool.Pool
	inputs map[string]map[string]string
}

func New() *Store {
	return &Store{runs: make(map[string]*Run), inputs: make(map[string]map[string]string)}
}

func (s *Store) InputStatus(runID, inputID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		var status string
		err := s.db.QueryRow(context.Background(), `SELECT status FROM agent_run_inputs WHERE run_id=$1 AND input_id=$2`, runID, inputID).Scan(&status)
		if err == pgx.ErrNoRows {
			return "", nil
		}
		return status, err
	}
	return s.inputs[runID][inputID], nil
}

func (s *Store) RecordInput(runID, inputID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[runID]; !ok {
		return fmt.Errorf("run %s not found", runID)
	}
	if s.db != nil {
		_, err := s.db.Exec(context.Background(), `INSERT INTO agent_run_inputs (run_id, input_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, runID, inputID)
		return err
	}
	if s.inputs[runID] == nil {
		s.inputs[runID] = make(map[string]string)
	}
	if s.inputs[runID][inputID] == "" {
		s.inputs[runID][inputID] = "accepted"
	}
	return nil
}

func (s *Store) FinishReceipt(runID, inputID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[runID]; !ok {
		return fmt.Errorf("run %s not found", runID)
	}
	if s.db != nil {
		_, err := s.db.Exec(context.Background(), `INSERT INTO agent_run_inputs (run_id,input_id,status) VALUES ($1,$2,$3)
			ON CONFLICT (run_id,input_id) DO UPDATE SET status=EXCLUDED.status,updated_at=NOW()`, runID, inputID, status)
		return err
	}
	if s.inputs[runID] == nil {
		s.inputs[runID] = make(map[string]string)
	}
	s.inputs[runID][inputID] = status
	return nil
}

// NewDB restores the API's run ownership and workflow index after a restart.
func NewDB(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	s := New()
	s.db = pool
	rows, err := pool.Query(ctx, `SELECT `+runColumns+` FROM agent_runs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		s.runs[r.ID] = r
	}
	return s, rows.Err()
}

const runColumns = `id, agent_name, owner_subject, status, response, error, task_id, workflow_id, session_id, persistent, created_at, mcp_servers, current_input_id, last_input_id, client_key, dispatch_key`

func scanRun(row pgx.Row) (*Run, error) {
	r := &Run{}
	if err := row.Scan(&r.ID, &r.AgentName, &r.Owner, &r.Status, &r.Response, &r.Error, &r.TaskID, &r.WorkflowID, &r.SessionID, &r.Persistent, &r.CreatedAt, &r.MCPServers, &r.CurrentInputID, &r.LastInputID, &r.ClientKey, &r.DispatchKey); err != nil {
		return nil, err
	}
	return r, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) loadConflict(owner, column, key string) (*Run, error) {
	query := `SELECT ` + runColumns + ` FROM agent_runs WHERE owner_subject=$1 AND ` + column + `=$2`
	if column == "client_key" {
		query += ` AND persistent AND status IN ('waiting', 'running')`
	}
	return scanRun(s.db.QueryRow(context.Background(), query, owner, key))
}

func (s *Store) persist(r *Run) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(context.Background(), `INSERT INTO agent_runs
		(id, agent_name, owner_subject, status, response, error, task_id, workflow_id, session_id, persistent, created_at, mcp_servers, current_input_id, last_input_id, client_key, dispatch_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status, response=EXCLUDED.response, error=EXCLUDED.error,
			workflow_id=EXCLUDED.workflow_id, persistent=EXCLUDED.persistent, mcp_servers=EXCLUDED.mcp_servers,
			current_input_id=EXCLUDED.current_input_id, last_input_id=EXCLUDED.last_input_id, client_key=EXCLUDED.client_key, dispatch_key=EXCLUDED.dispatch_key`,
		r.ID, r.AgentName, r.Owner, r.Status, r.Response, r.Error, r.TaskID, r.WorkflowID, r.SessionID, r.Persistent, r.CreatedAt, jsonOrEmpty(r.MCPServers), r.CurrentInputID, r.LastInputID, r.ClientKey, r.DispatchKey)
	return err
}

func jsonOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("[]")
	}
	return raw
}

func (s *Store) SetMCPServers(id string, data json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("run %s not found", id)
	}
	updated := *r
	updated.MCPServers = append(json.RawMessage(nil), data...)
	if err := s.persist(&updated); err != nil {
		return err
	}
	*r = updated
	return nil
}

func (s *Store) Create(agentName, owner, sessionID, taskID string) *Run {
	r := &Run{
		ID:        newID(),
		AgentName: agentName,
		Status:    StatusRunning,
		CreatedAt: time.Now(),
		Owner:     owner,
		SessionID: sessionID,
		TaskID:    taskID,
	}
	s.mu.Lock()
	s.runs[r.ID] = r
	s.mu.Unlock()
	return r
}

func (s *Store) CreateChecked(agentName, owner, sessionID, taskID string) (*Run, error) {
	r, _, err := s.CreateByTaskChecked(agentName, owner, sessionID, taskID)
	return r, err
}

func (s *Store) FindDispatchKey(owner, taskID string) (*Run, bool) {
	if taskID == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.Owner == owner && r.DispatchKey == taskID {
			copy := *r
			return &copy, true
		}
	}
	return nil, false
}

func (s *Store) CreateByTaskChecked(agentName, owner, sessionID, taskID string) (*Run, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if taskID != "" {
		for _, existing := range s.runs {
			if existing.Owner == owner && existing.DispatchKey == taskID {
				copy := *existing
				return &copy, false, nil
			}
		}
	}
	r := &Run{ID: newID(), AgentName: agentName, Owner: owner, SessionID: sessionID, TaskID: taskID, DispatchKey: taskID, Status: StatusRunning, CreatedAt: time.Now()}
	if err := s.persist(r); err != nil {
		if isUniqueViolation(err) && taskID != "" {
			if existing, lookupErr := s.loadConflict(owner, "dispatch_key", taskID); lookupErr == nil {
				s.runs[existing.ID] = existing
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	s.runs[r.ID] = r
	return r, true, nil
}

func (s *Store) Get(id string) (*Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, false
	}
	copy := *r
	return &copy, true
}

func (s *Store) InFlight() []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	var active []Run
	for _, r := range s.runs {
		if r.WorkflowID != "" && (r.Status == StatusRunning || r.Status == StatusWaiting) {
			active = append(active, *r)
		}
	}
	return active
}

func (s *Store) Unstarted() []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	var unstarted []Run
	for _, r := range s.runs {
		if r.WorkflowID == "" && (r.Status == StatusRunning || r.Status == StatusWaiting) {
			unstarted = append(unstarted, *r)
		}
	}
	return unstarted
}

// ListByTaskID returns all runs associated with a background task id (eve
// uses this to rediscover in-flight task runs after a restart).
func (s *Store) ListByTaskID(taskID string) []*Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Run, 0)
	for _, r := range s.runs {
		if r.TaskID == taskID {
			copy := *r
			out = append(out, &copy)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Store) UpdateStatus(id string, status Status, response, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("run %s not found", id)
	}
	if r.Status == StatusCanceled || r.Status == StatusCompleted || r.Status == StatusFailed {
		return ErrTerminal
	}
	updated := *r
	updated.Status = status
	updated.Response = response
	updated.Error = errMsg
	if err := s.persist(&updated); err != nil {
		return err
	}
	*r = updated
	return nil
}

func (s *Store) WaitForStatus(ctx context.Context, id string, status Status, response, errMsg string) error {
	for {
		if err := s.UpdateStatus(id, status, response, errMsg); err == nil {
			return nil
		} else if errors.Is(err, ErrTerminal) {
			return err
		} else {
			slog.Warn("retry recording agent run outcome", "run", id, "status", status, "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Store) BeginInput(id, inputID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || !r.Persistent || r.Status != StatusWaiting {
		return fmt.Errorf("run %s is not waiting for input", id)
	}
	updated := *r
	updated.Status = StatusRunning
	updated.Response = ""
	updated.Error = ""
	updated.CurrentInputID = inputID
	if err := s.persist(&updated); err != nil {
		return err
	}
	*r = updated
	return nil
}

func (s *Store) FinishInput(id, inputID string, response, errMsg string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return false, fmt.Errorf("run %s not found", id)
	}
	if r.CurrentInputID != inputID || r.Status != StatusRunning {
		return false, nil
	}
	updated := *r
	updated.Status = StatusWaiting
	updated.Response = response
	updated.Error = errMsg
	updated.CurrentInputID = ""
	updated.LastInputID = inputID
	if err := s.persist(&updated); err != nil {
		return false, err
	}
	*r = updated
	return true, nil
}

func (s *Store) ResetInput(id, inputID, response, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.CurrentInputID != inputID {
		return nil
	}
	updated := *r
	updated.Status = StatusWaiting
	updated.Response = response
	updated.Error = errMsg
	updated.CurrentInputID = ""
	if err := s.persist(&updated); err != nil {
		return err
	}
	*r = updated
	return nil
}

func (s *Store) SetWorkflowID(id, workflowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("run %s not found", id)
	}
	updated := *r
	updated.WorkflowID = workflowID
	if err := s.persist(&updated); err != nil {
		return err
	}
	*r = updated
	return nil
}

func (s *Store) SetEphemeralNames(id string, names []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("run %s not found", id)
	}
	r.EphemeralNames = append([]string(nil), names...)
	return nil
}

func (s *Store) AddEphemeralNameIfActive(id, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil || (r.Status != StatusRunning && r.Status != StatusWaiting) {
		return false
	}
	r.EphemeralNames = append(r.EphemeralNames, name)
	return true
}

func (s *Store) Delete(id string) {
	s.mu.Lock()
	if s.db != nil {
		if _, err := s.db.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id); err != nil {
			s.mu.Unlock()
			return
		}
	}
	delete(s.runs, id)
	delete(s.inputs, id)
	s.mu.Unlock()
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("generate run ID: %w", err))
	}
	return hex.EncodeToString(b)
}
