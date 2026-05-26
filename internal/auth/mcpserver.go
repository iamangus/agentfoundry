package auth

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

var ErrMCPServerNotFound = errors.New("mcp server not found")
var ErrMCPServerNameTaken = errors.New("mcp server name already taken")

type MCPServerRecord struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Transport string            `json:"transport"`
	Headers   map[string]string `json:"headers"`
	Scope     string            `json:"scope"`
	Team      string            `json:"team,omitempty"`
	CreatedBy string            `json:"created_by"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type ToolScopeEntry struct {
	ServerID  string `json:"-"`
	ToolName  string `json:"tool_name"`
	Scope     string `json:"scope"`
	Team      string `json:"team,omitempty"`
	CreatedBy string `json:"created_by"`
}

type MCPServerStore struct {
	db *pgxpool.Pool
}

func NewMCPServerStore(db *pgxpool.Pool) *MCPServerStore {
	return &MCPServerStore{db: db}
}

func (s *MCPServerStore) Create(ctx context.Context, name, url, transport, scope, team, createdBy string, headers map[string]string) (*MCPServerRecord, error) {
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("marshal headers: %w", err)
	}

	var rec MCPServerRecord
	var updatedAt time.Time

	err = s.db.QueryRow(ctx,
		`INSERT INTO mcp_servers (name, url, transport, headers, scope, team, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, name, url, transport, headers, scope, team, created_by, created_at, updated_at`,
		name, url, transport, headersJSON, scope, team, createdBy,
	).Scan(&rec.ID, &rec.Name, &rec.URL, &rec.Transport, &headersJSON, &rec.Scope, &rec.Team, &rec.CreatedBy, &rec.CreatedAt, &updatedAt)
	if err != nil {
		if isPgUniqueViolation(err) {
			return nil, ErrMCPServerNameTaken
		}
		return nil, fmt.Errorf("insert mcp server: %w", err)
	}

	rec.UpdatedAt = updatedAt
	_ = json.Unmarshal(headersJSON, &rec.Headers)
	return &rec, nil
}

func (s *MCPServerStore) GetByName(ctx context.Context, name string) (*MCPServerRecord, error) {
	var rec MCPServerRecord
	var headersJSON []byte
	var team *string

	err := s.db.QueryRow(ctx,
		`SELECT id, name, url, transport, headers, scope, team, created_by, created_at, updated_at FROM mcp_servers WHERE name = $1`,
		name,
	).Scan(&rec.ID, &rec.Name, &rec.URL, &rec.Transport, &headersJSON, &rec.Scope, &team, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrMCPServerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get mcp server: %w", err)
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

func (s *MCPServerStore) List(ctx context.Context, subject string, teams []string, isGlobalAdmin bool) ([]MCPServerRecord, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, name, url, transport, headers, scope, team, created_by, created_at, updated_at FROM mcp_servers ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers: %w", err)
	}
	defer rows.Close()

	var results []MCPServerRecord
	for rows.Next() {
		var rec MCPServerRecord
		var headersJSON []byte
		var team *string

		if err := rows.Scan(&rec.ID, &rec.Name, &rec.URL, &rec.Transport, &headersJSON, &rec.Scope, &team, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan mcp server: %w", err)
		}

		if team != nil {
			rec.Team = *team
		}

		if visibleTo(rec.Scope, rec.Team, rec.CreatedBy, subject, teams, isGlobalAdmin) {
			_ = json.Unmarshal(headersJSON, &rec.Headers)
			if rec.Headers == nil {
				rec.Headers = map[string]string{}
			}
			results = append(results, rec)
		}
	}

	return results, nil
}

func (s *MCPServerStore) ListAll(ctx context.Context) ([]MCPServerRecord, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, name, url, transport, headers, scope, team, created_by, created_at, updated_at FROM mcp_servers ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all mcp servers: %w", err)
	}
	defer rows.Close()

	var results []MCPServerRecord
	for rows.Next() {
		var rec MCPServerRecord
		var headersJSON []byte
		var team *string

		if err := rows.Scan(&rec.ID, &rec.Name, &rec.URL, &rec.Transport, &headersJSON, &rec.Scope, &team, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan mcp server: %w", err)
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

func visibleTo(scope, team, createdBy, subject string, teams []string, isGlobalAdmin bool) bool {
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

func (s *MCPServerStore) CanEdit(subject string, teams []string, isGlobalAdmin, isTeamAdmin bool, serverName string) (bool, *MCPServerRecord, error) {
	rec, err := s.GetByName(context.Background(), serverName)
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

func (s *MCPServerStore) GetByID(ctx context.Context, id string) (*MCPServerRecord, error) {
	var rec MCPServerRecord
	var headersJSON []byte
	var team *string

	err := s.db.QueryRow(ctx,
		`SELECT id, name, url, transport, headers, scope, team, created_by, created_at, updated_at FROM mcp_servers WHERE id = $1`,
		id,
	).Scan(&rec.ID, &rec.Name, &rec.URL, &rec.Transport, &headersJSON, &rec.Scope, &team, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrMCPServerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get mcp server by id: %w", err)
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

func (s *MCPServerStore) Update(ctx context.Context, id, name, url, transport, scope, team string, headers map[string]string) (*MCPServerRecord, error) {
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		return nil, fmt.Errorf("marshal headers: %w", err)
	}

	var rec MCPServerRecord
	var hdrJSON []byte
	var dbTeam *string

	err = s.db.QueryRow(ctx,
		`UPDATE mcp_servers SET name=$1, url=$2, transport=$3, headers=$4, scope=$5, team=$6, updated_at=NOW() WHERE id=$7 RETURNING id, name, url, transport, headers, scope, team, created_by, created_at, updated_at`,
		name, url, transport, headersJSON, scope, team, id,
	).Scan(&rec.ID, &rec.Name, &rec.URL, &rec.Transport, &hdrJSON, &rec.Scope, &dbTeam, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrMCPServerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update mcp server: %w", err)
	}

	if dbTeam != nil {
		rec.Team = *dbTeam
	}
	_ = json.Unmarshal(hdrJSON, &rec.Headers)
	if rec.Headers == nil {
		rec.Headers = map[string]string{}
	}
	return &rec, nil
}

func (s *MCPServerStore) Delete(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM mcp_servers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete mcp server: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMCPServerNotFound
	}
	return nil
}

func (s *MCPServerStore) SetToolScope(ctx context.Context, serverID, toolName, scope, team, createdBy string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO mcp_tool_scopes (server_id, tool_name, scope, team, created_by) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (server_id, tool_name) DO UPDATE SET scope=$3, team=$4, created_by=$5`,
		serverID, toolName, scope, team, createdBy,
	)
	if err != nil {
		return fmt.Errorf("set tool scope: %w", err)
	}
	return nil
}

func (s *MCPServerStore) RemoveToolScope(ctx context.Context, serverID, toolName string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM mcp_tool_scopes WHERE server_id = $1 AND tool_name = $2`,
		serverID, toolName,
	)
	if err != nil {
		return fmt.Errorf("remove tool scope: %w", err)
	}
	return nil
}

func (s *MCPServerStore) GetToolScopes(ctx context.Context, serverID string) (map[string]ToolScopeEntry, error) {
	rows, err := s.db.Query(ctx,
		`SELECT tool_name, scope, team, created_by FROM mcp_tool_scopes WHERE server_id = $1`,
		serverID,
	)
	if err != nil {
		return nil, fmt.Errorf("get tool scopes: %w", err)
	}
	defer rows.Close()

	result := make(map[string]ToolScopeEntry)
	for rows.Next() {
		var e ToolScopeEntry
		if err := rows.Scan(&e.ToolName, &e.Scope, &e.Team, &e.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan tool scope: %w", err)
		}
		e.ServerID = serverID
		result[e.ToolName] = e
	}
	return result, nil
}

func isPgUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "violates unique constraint") || strings.Contains(msg, "duplicate key")
}
