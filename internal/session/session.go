package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Message struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Time    time.Time `json:"time"`
}

type Session struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	AgentName   string    `json:"agent_name"`
	Messages    []Message `json:"messages"`
	CreatedAt   time.Time `json:"created_at"`
	ActiveRunID string    `json:"active_run_id,omitempty"`
	Owner       string    `json:"owner"`
}

type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
	db       *pgxpool.Pool
}

func NewDB(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	s := New()
	s.db = pool
	rows, err := pool.Query(ctx, `SELECT id, agent_id, agent_name, owner_subject, messages, active_run_id, created_at FROM chat_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sess Session
		var data []byte
		if err := rows.Scan(&sess.ID, &sess.AgentID, &sess.AgentName, &sess.Owner, &data, &sess.ActiveRunID, &sess.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &sess.Messages); err != nil {
			return nil, err
		}
		s.sessions[sess.ID] = &sess
	}
	return s, rows.Err()
}

func (s *Store) persist(sess *Session) error {
	if s.db == nil {
		return nil
	}
	data, err := json.Marshal(sess.Messages)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(context.Background(), `INSERT INTO chat_sessions
		(id,agent_id,agent_name,owner_subject,messages,active_run_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (id) DO UPDATE SET messages=EXCLUDED.messages,active_run_id=EXCLUDED.active_run_id`,
		sess.ID, sess.AgentID, sess.AgentName, sess.Owner, data, sess.ActiveRunID, sess.CreatedAt)
	return err
}

func New() *Store {
	return &Store{sessions: make(map[string]*Session)}
}

func (s *Store) Create(agentID, agentName, owner string) *Session {
	id := newID()
	sess := &Session{
		ID:        id,
		AgentID:   agentID,
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

func (s *Store) CreateChecked(agentID, agentName, owner string) (*Session, error) {
	sess := &Session{ID: newID(), AgentID: agentID, AgentName: agentName, Messages: []Message{}, CreatedAt: time.Now(), Owner: owner}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persist(sess); err != nil {
		return nil, err
	}
	s.sessions[sess.ID] = sess
	return sess, nil
}

func (s *Store) Get(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.sessions[id])
}

func clone(sess *Session) *Session {
	if sess == nil {
		return nil
	}
	copy := *sess
	copy.Messages = append([]Message(nil), sess.Messages...)
	return &copy
}

func (s *Store) ListByOwner(owner string) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if sess.Owner == owner {
			out = append(out, clone(sess))
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
		out = append(out, clone(sess))
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
	updated := clone(sess)
	updated.Messages = append(updated.Messages, msg)
	if err := s.persist(updated); err != nil {
		return err
	}
	*sess = *updated
	return nil
}

func (s *Store) SetActiveRunID(id, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session not found")
	}
	updated := clone(sess)
	updated.ActiveRunID = runID
	if err := s.persist(updated); err != nil {
		return err
	}
	*sess = *updated
	return nil
}

func (s *Store) StartRun(id, runID string, msg Message) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		return nil, fmt.Errorf("session not found")
	}
	if sess.ActiveRunID != "" {
		return nil, fmt.Errorf("session already has an active run")
	}
	previous := append([]Message(nil), sess.Messages...)
	updated := clone(sess)
	updated.Messages = append(updated.Messages, msg)
	updated.ActiveRunID = runID
	if err := s.persist(updated); err != nil {
		return nil, err
	}
	*sess = *updated
	return previous, nil
}

func (s *Store) ClearActiveRunID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	updated := clone(sess)
	updated.ActiveRunID = ""
	if err := s.persist(updated); err != nil {
		return err
	}
	*sess = *updated
	return nil
}

func (s *Store) CompleteRun(id, runID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil || sess.ActiveRunID != runID {
		return nil
	}
	updated := clone(sess)
	if len(updated.Messages) == 0 || updated.Messages[len(updated.Messages)-1].Role != "assistant" {
		updated.Messages = append(updated.Messages, Message{Role: "assistant", Content: text, Time: time.Now()})
	}
	updated.ActiveRunID = ""
	if err := s.persist(updated); err != nil {
		return err
	}
	*sess = *updated
	return nil
}

func (s *Store) CompleteRunUntil(ctx context.Context, id, runID, text string) error {
	for {
		if err := s.CompleteRun(id, runID, text); err == nil {
			return nil
		} else {
			slog.Warn("retry completing chat session", "session", id, "run", runID, "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Store) FindByRunID(runID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ActiveRunID == runID {
			return clone(sess)
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("generate session ID: %w", err))
	}
	return hex.EncodeToString(b)
}
