package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrProviderNotFound = errors.New("inference provider not found")
var ErrProviderNameTaken = errors.New("inference provider name already taken")

type ProviderRecord struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	ProviderType     string            `json:"provider_type"`
	BaseURL          string            `json:"base_url"`
	APIKey           string            `json:"api_key,omitempty"`
	DefaultModel     string            `json:"default_model"`
	SchemaValidation bool              `json:"schema_validation"`
	Headers          map[string]string `json:"headers"`
	Scope            string            `json:"scope"`
	Team             string            `json:"team,omitempty"`
	CreatedBy        string            `json:"created_by"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type ProviderStore struct {
	db *pgxpool.Pool
}

func NewProviderStore(db *pgxpool.Pool) *ProviderStore {
	return &ProviderStore{db: db}
}

func (s *ProviderStore) Create(ctx context.Context, name, providerType, baseURL, apiKey, defaultModel string, schemaValidation bool, scope, team, createdBy string, headers map[string]string) (*ProviderRecord, error) {
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("marshal headers: %w", err)
	}

	var rec ProviderRecord
	var updatedAt time.Time

	err = s.db.QueryRow(ctx,
		`INSERT INTO inference_providers (name, provider_type, base_url, api_key, default_model, schema_validation, headers, scope, team, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id, name, provider_type, base_url, api_key, default_model, schema_validation, headers, scope, team, created_by, created_at, updated_at`,
		name, providerType, baseURL, apiKey, defaultModel, schemaValidation, headersJSON, scope, team, createdBy,
	).Scan(&rec.ID, &rec.Name, &rec.ProviderType, &rec.BaseURL, &rec.APIKey, &rec.DefaultModel, &rec.SchemaValidation, &headersJSON, &rec.Scope, &rec.Team, &rec.CreatedBy, &rec.CreatedAt, &updatedAt)
	if err != nil {
		if isPgUniqueViolation(err) {
			return nil, ErrProviderNameTaken
		}
		return nil, fmt.Errorf("insert inference provider: %w", err)
	}

	rec.UpdatedAt = updatedAt
	_ = json.Unmarshal(headersJSON, &rec.Headers)
	return &rec, nil
}

func (s *ProviderStore) GetByID(ctx context.Context, id string) (*ProviderRecord, error) {
	var rec ProviderRecord
	var headersJSON []byte
	var team *string

	err := s.db.QueryRow(ctx,
		`SELECT id, name, provider_type, base_url, api_key, default_model, schema_validation, headers, scope, team, created_by, created_at, updated_at FROM inference_providers WHERE id = $1`,
		id,
	).Scan(&rec.ID, &rec.Name, &rec.ProviderType, &rec.BaseURL, &rec.APIKey, &rec.DefaultModel, &rec.SchemaValidation, &headersJSON, &rec.Scope, &team, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get inference provider: %w", err)
	}

	if team != nil {
		rec.Team = *team
	}
	_ = json.Unmarshal(headersJSON, &rec.Headers)
	if rec.Headers == nil {
		rec.Headers = map[string]string{}
	}
	return &rec, nil
}

func (s *ProviderStore) GetByName(ctx context.Context, name string) (*ProviderRecord, error) {
	var rec ProviderRecord
	var headersJSON []byte
	var team *string

	err := s.db.QueryRow(ctx,
		`SELECT id, name, provider_type, base_url, api_key, default_model, schema_validation, headers, scope, team, created_by, created_at, updated_at FROM inference_providers WHERE name = $1`,
		name,
	).Scan(&rec.ID, &rec.Name, &rec.ProviderType, &rec.BaseURL, &rec.APIKey, &rec.DefaultModel, &rec.SchemaValidation, &headersJSON, &rec.Scope, &team, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get inference provider: %w", err)
	}

	if team != nil {
		rec.Team = *team
	}
	_ = json.Unmarshal(headersJSON, &rec.Headers)
	if rec.Headers == nil {
		rec.Headers = map[string]string{}
	}
	return &rec, nil
}

func (s *ProviderStore) List(ctx context.Context, subject string, teams []string, isGlobalAdmin bool) ([]ProviderRecord, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, name, provider_type, base_url, api_key, default_model, schema_validation, headers, scope, team, created_by, created_at, updated_at FROM inference_providers ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list inference providers: %w", err)
	}
	defer rows.Close()

	var results []ProviderRecord
	for rows.Next() {
		var rec ProviderRecord
		var headersJSON []byte
		var team *string

		if err := rows.Scan(&rec.ID, &rec.Name, &rec.ProviderType, &rec.BaseURL, &rec.APIKey, &rec.DefaultModel, &rec.SchemaValidation, &headersJSON, &rec.Scope, &team, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan inference provider: %w", err)
		}

		if team != nil {
			rec.Team = *team
		}

		if s.visibleTo(rec.Scope, rec.Team, rec.CreatedBy, subject, teams, isGlobalAdmin) {
			_ = json.Unmarshal(headersJSON, &rec.Headers)
			if rec.Headers == nil {
				rec.Headers = map[string]string{}
			}
			results = append(results, rec)
		}
	}

	return results, nil
}

func (s *ProviderStore) ListAll(ctx context.Context) ([]ProviderRecord, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, name, provider_type, base_url, api_key, default_model, schema_validation, headers, scope, team, created_by, created_at, updated_at FROM inference_providers ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all inference providers: %w", err)
	}
	defer rows.Close()

	var results []ProviderRecord
	for rows.Next() {
		var rec ProviderRecord
		var headersJSON []byte
		var team *string

		if err := rows.Scan(&rec.ID, &rec.Name, &rec.ProviderType, &rec.BaseURL, &rec.APIKey, &rec.DefaultModel, &rec.SchemaValidation, &headersJSON, &rec.Scope, &team, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan inference provider: %w", err)
		}

		if team != nil {
			rec.Team = *team
		}
		_ = json.Unmarshal(headersJSON, &rec.Headers)
		if rec.Headers == nil {
			rec.Headers = map[string]string{}
		}
		results = append(results, rec)
	}

	return results, nil
}

func (s *ProviderStore) Update(ctx context.Context, id, name, providerType, baseURL, apiKey, defaultModel string, schemaValidation bool, scope, team string, headers map[string]string) (*ProviderRecord, error) {
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("marshal headers: %w", err)
	}

	var rec ProviderRecord
	var updatedAt time.Time

	err = s.db.QueryRow(ctx,
		`UPDATE inference_providers SET name = $1, provider_type = $2, base_url = $3, api_key = $4, default_model = $5, schema_validation = $6, headers = $7, scope = $8, team = $9, updated_at = NOW() WHERE id = $10 RETURNING id, name, provider_type, base_url, api_key, default_model, schema_validation, headers, scope, team, created_by, created_at, updated_at`,
		name, providerType, baseURL, apiKey, defaultModel, schemaValidation, headersJSON, scope, team, id,
	).Scan(&rec.ID, &rec.Name, &rec.ProviderType, &rec.BaseURL, &rec.APIKey, &rec.DefaultModel, &rec.SchemaValidation, &headersJSON, &rec.Scope, &rec.Team, &rec.CreatedBy, &rec.CreatedAt, &updatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update inference provider: %w", err)
	}

	rec.UpdatedAt = updatedAt
	_ = json.Unmarshal(headersJSON, &rec.Headers)
	return &rec, nil
}

func (s *ProviderStore) Delete(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM inference_providers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete inference provider: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProviderNotFound
	}
	return nil
}

func (s *ProviderStore) CanEdit(ctx context.Context, subject string, teams []string, isGlobalAdmin, isTeamAdmin bool, providerName string) (bool, *ProviderRecord, error) {
	rec, err := s.GetByName(ctx, providerName)
	if err != nil {
		return false, nil, err
	}

	switch rec.Scope {
	case "global":
		return isGlobalAdmin || rec.CreatedBy == subject, rec, nil
	case "team":
		if isGlobalAdmin {
			return true, rec, nil
		}
		if rec.CreatedBy == subject {
			return true, rec, nil
		}
		if isTeamAdmin {
			for _, t := range teams {
				if t == rec.Team {
					return true, rec, nil
				}
			}
		}
		return false, rec, nil
	case "user", "":
		return rec.CreatedBy == subject, rec, nil
	default:
		return false, rec, nil
	}
}

func (s *ProviderStore) CanDelete(subject string, teams []string, isGlobalAdmin, isTeamAdmin bool, providerName string) (bool, error) {
	canEdit, _, err := s.CanEdit(context.Background(), subject, teams, isGlobalAdmin, isTeamAdmin, providerName)
	if err != nil {
		return false, err
	}
	return canEdit, nil
}

func (s *ProviderStore) visibleTo(scope, team, createdBy, subject string, teams []string, isGlobalAdmin bool) bool {
	switch scope {
	case "global":
		return true
	case "team":
		if isGlobalAdmin {
			return true
		}
		for _, t := range teams {
			if t == team {
				return true
			}
		}
		return false
	case "user", "":
		return subject != "" && createdBy == subject
	default:
		return false
	}
}

func isPgUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "violates unique constraint") || strings.Contains(msg, "duplicate key")
}
