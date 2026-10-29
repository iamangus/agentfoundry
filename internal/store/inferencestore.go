package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type InferenceMetrics struct {
	InputTokens                   *int64   `json:"input_tokens,omitempty"`
	OutputTokens                  *int64   `json:"output_tokens,omitempty"`
	CachedInputTokens             *int64   `json:"cached_input_tokens,omitempty"`
	ReasoningTokens               *int64   `json:"reasoning_tokens,omitempty"`
	CacheHitRatio                 *float64 `json:"cache_hit_ratio,omitempty"`
	DurationMS                    float64  `json:"duration_ms"`
	TimeToFirstTokenMS            *float64 `json:"time_to_first_token_ms,omitempty"`
	TimeToFirstVisibleTokenMS     *float64 `json:"time_to_first_visible_token_ms,omitempty"`
	ObservedOutputTokensPerSecond *float64 `json:"observed_output_tokens_per_second,omitempty"`
	PrefillMS                     *float64 `json:"prefill_ms,omitempty"`
	PrefillTokensPerSecond        *float64 `json:"prefill_tokens_per_second,omitempty"`
	TimeToStartPrefillMS          *float64 `json:"time_to_start_prefill_ms,omitempty"`
	ProviderDecodeMS              *float64 `json:"provider_decode_ms,omitempty"`
	TimingSource                  string   `json:"timing_source"`
	ProviderTimingSource          string   `json:"provider_timing_source,omitempty"`
}

type InferenceAttempt struct {
	ID              string           `json:"id"`
	RunID           string           `json:"run_id,omitempty"`
	InputID         string           `json:"input_id,omitempty"`
	AgentID         string           `json:"agent_id"`
	ProviderID      string           `json:"provider_id"`
	Model           string           `json:"model"`
	Backup          bool             `json:"backup"`
	WorkflowID      string           `json:"workflow_id,omitempty"`
	ExecutionID     string           `json:"execution_id,omitempty"`
	ActivityID      string           `json:"activity_id,omitempty"`
	ActivityAttempt int              `json:"activity_attempt"`
	HTTPAttempt     int              `json:"http_attempt"`
	StartedAt       time.Time        `json:"started_at"`
	FinishedAt      time.Time        `json:"finished_at"`
	Status          string           `json:"status"`
	HTTPStatus      int              `json:"http_status"`
	Metrics         InferenceMetrics `json:"metrics"`
}

type InferencePage struct {
	Attempts   []InferenceAttempt `json:"attempts"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type inferenceCursor struct {
	StartedAt time.Time `json:"started_at"`
	ID        string    `json:"id"`
}
type InferenceStore struct{ pool *pgxpool.Pool }

func NewInferenceStore(pool *pgxpool.Pool) *InferenceStore { return &InferenceStore{pool: pool} }

func (s *InferenceStore) Save(ctx context.Context, attempt InferenceAttempt) error {
	attempt.StartedAt = attempt.StartedAt.UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO inference_attempts (id, run_id, input_id, agent_id, provider_id, started_at, payload) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (id) DO UPDATE SET run_id=EXCLUDED.run_id,input_id=EXCLUDED.input_id,agent_id=EXCLUDED.agent_id,provider_id=EXCLUDED.provider_id,started_at=EXCLUDED.started_at,payload=EXCLUDED.payload`, attempt.ID, attempt.RunID, attempt.InputID, attempt.AgentID, attempt.ProviderID, attempt.StartedAt, payload)
	return err
}

func (s *InferenceStore) List(ctx context.Context, runID, inputID, cursor string) (InferencePage, error) {
	page := InferencePage{Attempts: []InferenceAttempt{}}
	var before inferenceCursor
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(b, &before) != nil || before.ID == "" || before.StartedAt.IsZero() {
			return page, fmt.Errorf("invalid inference cursor")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT payload FROM inference_attempts WHERE run_id=$1 AND ($2='' OR input_id=$2) AND ($3::timestamptz IS NULL OR (started_at,id)<($3::timestamptz,$4)) ORDER BY started_at DESC,id DESC LIMIT 201`, runID, inputID, nullableCursorTime(before.StartedAt), before.ID)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var attempt InferenceAttempt
		if err = rows.Scan(&raw); err != nil {
			return page, err
		}
		if err = json.Unmarshal(raw, &attempt); err != nil {
			return page, err
		}
		page.Attempts = append(page.Attempts, attempt)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Attempts) > 200 {
		page.Attempts = page.Attempts[:200]
		last := page.Attempts[len(page.Attempts)-1]
		b, _ := json.Marshal(inferenceCursor{StartedAt: last.StartedAt, ID: last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}

func nullableCursorTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
