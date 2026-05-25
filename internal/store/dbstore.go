package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/angoo/agentfoundry/internal/config"
)

type AgentRegistrar interface {
	RegisterAgent(def *config.Definition) error
}

type DBStore struct {
	pool  *pgxpool.Pool
	reg   AgentRegistrar
}

func NewDBStore(pool *pgxpool.Pool, reg AgentRegistrar) *DBStore {
	return &DBStore{
		pool: pool,
		reg:  reg,
	}
}

func (s *DBStore) LoadAll(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT agent_id, name, kind, description, model, system_prompt, tools,
			max_turns, max_concurrent_tools, force_json, structured_output, scope, team, created_by, provider_id
			FROM agent_definitions ORDER BY name`)
	if err != nil {
		return fmt.Errorf("query agent definitions: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var row struct {
			AgentID            string
			Name               string
			Kind               string
			Description        string
			Model              string
			SystemPrompt       string
			Tools              []byte
			MaxTurns           int
			MaxConcurrentTools int
			ForceJSON          bool
			StructuredOutput   []byte
			Scope              string
			Team               string
			CreatedBy          string
			ProviderID         string
		}
		if err := rows.Scan(&row.AgentID, &row.Name, &row.Kind, &row.Description, &row.Model,
			&row.SystemPrompt, &row.Tools, &row.MaxTurns, &row.MaxConcurrentTools, &row.ForceJSON,
			&row.StructuredOutput, &row.Scope, &row.Team, &row.CreatedBy, &row.ProviderID); err != nil {
			slog.Error("failed to scan agent definition row", "error", err)
			continue
		}

		def := &config.Definition{
			AgentID:            row.AgentID,
			Kind:               config.Kind(row.Kind),
			Name:               row.Name,
			Description:        row.Description,
			Model:              row.Model,
			SystemPrompt:       row.SystemPrompt,
			MaxTurns:           row.MaxTurns,
			MaxConcurrentTools: row.MaxConcurrentTools,
			ForceJSON:          row.ForceJSON,
			Scope:              row.Scope,
			Team:               row.Team,
			CreatedBy:          row.CreatedBy,
			ProviderID:         row.ProviderID,
		}

		if len(row.Tools) > 0 {
			if err := json.Unmarshal(row.Tools, &def.Tools); err != nil {
				slog.Error("failed to unmarshal tools", "name", row.Name, "error", err)
			}
		}

		if len(row.StructuredOutput) > 0 {
			var so config.StructuredOutput
			if err := json.Unmarshal(row.StructuredOutput, &so); err != nil {
				slog.Error("failed to unmarshal structured_output", "name", row.Name, "error", err)
			} else {
				def.StructuredOutput = &so
			}
		}

		if err := s.reg.RegisterAgent(def); err != nil {
			slog.Error("failed to register agent from db", "name", row.Name, "error", err)
			continue
		}
		count++
	}

	slog.Info("agent definitions loaded from database", "count", count)
	return rows.Err()
}

func (s *DBStore) SaveDefinition(def *config.Definition) error {
	ctx := context.Background()

	if def.AgentID == "" {
		var existingID string
		if err := s.pool.QueryRow(context.Background(),
			`SELECT agent_id FROM agent_definitions WHERE name = $1`, def.Name).Scan(&existingID); err == nil {
			def.AgentID = existingID
		} else {
			def.AgentID = generateAgentID()
		}
	}

	toolsJSON, err := json.Marshal(def.Tools)
	if err != nil {
		return fmt.Errorf("marshal tools: %w", err)
	}
	if toolsJSON == nil {
		toolsJSON = []byte("[]")
	}

	var soJSON []byte
	if def.StructuredOutput != nil {
		soJSON, err = json.Marshal(def.StructuredOutput)
		if err != nil {
			return fmt.Errorf("marshal structured_output: %w", err)
		}
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO agent_definitions
			(agent_id, name, kind, description, model, system_prompt, tools,
			 max_turns, max_concurrent_tools, force_json, structured_output,
			 scope, team, created_by, provider_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, NOW())
		ON CONFLICT (name) DO UPDATE SET
			description         = EXCLUDED.description,
			model               = EXCLUDED.model,
			system_prompt       = EXCLUDED.system_prompt,
			tools               = EXCLUDED.tools,
			max_turns           = EXCLUDED.max_turns,
			max_concurrent_tools = EXCLUDED.max_concurrent_tools,
			force_json          = EXCLUDED.force_json,
			structured_output   = EXCLUDED.structured_output,
			scope               = EXCLUDED.scope,
			team                = EXCLUDED.team,
			provider_id         = EXCLUDED.provider_id,
			updated_at          = NOW()
	`, def.AgentID, def.Name, string(def.Kind), def.Description, def.Model,
		def.SystemPrompt, toolsJSON, def.MaxTurns, def.MaxConcurrentTools,
		def.ForceJSON, soJSON, def.Scope, def.Team, def.CreatedBy, def.ProviderID,
	)
	if err != nil {
		return fmt.Errorf("save agent definition: %w", err)
	}

	yamlData, err := yaml.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal yaml for version: %w", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO agent_versions (agent_id, definition_yaml) VALUES ($1, $2)`, def.AgentID, string(yamlData))
	if err != nil {
		slog.Error("failed to record agent version", "agent_id", def.AgentID, "error", err)
	}

	return s.reg.RegisterAgent(def)
}

func (s *DBStore) DeleteDefinition(name string) error {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `DELETE FROM agent_definitions WHERE name = $1`, name)
	return err
}

func (s *DBStore) GetDefinition(name string) *config.Definition {
	ctx := context.Background()

	var row struct {
		AgentID            string
		Name               string
		Kind               string
		Description        string
		Model              string
		SystemPrompt       string
		Tools              []byte
		MaxTurns           int
		MaxConcurrentTools int
		ForceJSON          bool
		StructuredOutput   []byte
		Scope              string
		Team               string
		CreatedBy          string
		ProviderID         string
	}
	err := s.pool.QueryRow(ctx, `
		SELECT agent_id, name, kind, description, model, system_prompt, tools,
			max_turns, max_concurrent_tools, force_json, structured_output, scope, team, created_by, provider_id
		FROM agent_definitions WHERE name = $1`, name).
		Scan(&row.AgentID, &row.Name, &row.Kind, &row.Description, &row.Model,
			&row.SystemPrompt, &row.Tools, &row.MaxTurns, &row.MaxConcurrentTools, &row.ForceJSON,
			&row.StructuredOutput, &row.Scope, &row.Team, &row.CreatedBy, &row.ProviderID)
	if err != nil {
		return nil
	}

	def := &config.Definition{
		AgentID:            row.AgentID,
		Kind:               config.Kind(row.Kind),
		Name:               row.Name,
		Description:        row.Description,
		Model:              row.Model,
		SystemPrompt:       row.SystemPrompt,
		MaxTurns:           row.MaxTurns,
		MaxConcurrentTools: row.MaxConcurrentTools,
		ForceJSON:          row.ForceJSON,
		Scope:              row.Scope,
		Team:               row.Team,
		CreatedBy:          row.CreatedBy,
	}

	if len(row.Tools) > 0 {
		json.Unmarshal(row.Tools, &def.Tools)
	}

	if len(row.StructuredOutput) > 0 {
		var so config.StructuredOutput
		if err := json.Unmarshal(row.StructuredOutput, &so); err == nil {
			def.StructuredOutput = &so
		}
	}

	return def
}

func (s *DBStore) GetDefinitionByID(agentID string) *config.Definition {
	ctx := context.Background()

	var row struct {
		AgentID            string
		Name               string
		Kind               string
		Description        string
		Model              string
		SystemPrompt       string
		Tools              []byte
		MaxTurns           int
		MaxConcurrentTools int
		ForceJSON          bool
		StructuredOutput   []byte
		Scope              string
		Team               string
		CreatedBy          string
		ProviderID         string
	}
	err := s.pool.QueryRow(ctx, `
		SELECT agent_id, name, kind, description, model, system_prompt, tools,
			max_turns, max_concurrent_tools, force_json, structured_output, scope, team, created_by, provider_id
		FROM agent_definitions WHERE agent_id = $1`, agentID).
		Scan(&row.AgentID, &row.Name, &row.Kind, &row.Description, &row.Model,
			&row.SystemPrompt, &row.Tools, &row.MaxTurns, &row.MaxConcurrentTools, &row.ForceJSON,
			&row.StructuredOutput, &row.Scope, &row.Team, &row.CreatedBy, &row.ProviderID)
	if err != nil {
		return nil
	}

	def := &config.Definition{
		AgentID:            row.AgentID,
		Kind:               config.Kind(row.Kind),
		Name:               row.Name,
		Description:        row.Description,
		Model:              row.Model,
		SystemPrompt:       row.SystemPrompt,
		MaxTurns:           row.MaxTurns,
		MaxConcurrentTools: row.MaxConcurrentTools,
		ForceJSON:          row.ForceJSON,
		Scope:              row.Scope,
		Team:               row.Team,
		CreatedBy:          row.CreatedBy,
		ProviderID:         row.ProviderID,
	}

	if len(row.Tools) > 0 {
		json.Unmarshal(row.Tools, &def.Tools)
	}

	if len(row.StructuredOutput) > 0 {
		var so config.StructuredOutput
		if err := json.Unmarshal(row.StructuredOutput, &so); err == nil {
			def.StructuredOutput = &so
		}
	}

	return def
}

func (s *DBStore) ListDefinitions() []*config.Definition {
	defs := make([]*config.Definition, 0)

	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT agent_id, name, kind, description, model, system_prompt, tools,
		max_turns, max_concurrent_tools, force_json, structured_output, scope, team, created_by
		FROM agent_definitions ORDER BY name`)
	if err != nil {
		slog.Error("failed to list agent definitions", "error", err)
		return defs
	}
	defer rows.Close()

	for rows.Next() {
		var row struct {
			AgentID            string
			Name               string
			Kind               string
			Description        string
			Model              string
			SystemPrompt       string
			Tools              []byte
			MaxTurns           int
			MaxConcurrentTools int
			ForceJSON          bool
			StructuredOutput   []byte
			Scope              string
			Team               string
			CreatedBy          string
		}
		if err := rows.Scan(&row.AgentID, &row.Name, &row.Kind, &row.Description, &row.Model,
			&row.SystemPrompt, &row.Tools, &row.MaxTurns, &row.MaxConcurrentTools, &row.ForceJSON,
			&row.StructuredOutput, &row.Scope, &row.Team, &row.CreatedBy); err != nil {
			slog.Error("failed to scan agent definition row", "error", err)
			continue
		}

		def := &config.Definition{
			AgentID:            row.AgentID,
			Kind:               config.Kind(row.Kind),
			Name:               row.Name,
			Description:        row.Description,
			Model:              row.Model,
			SystemPrompt:       row.SystemPrompt,
			MaxTurns:           row.MaxTurns,
			MaxConcurrentTools: row.MaxConcurrentTools,
			ForceJSON:          row.ForceJSON,
			Scope:              row.Scope,
			Team:               row.Team,
			CreatedBy:          row.CreatedBy,
		}

		if len(row.Tools) > 0 {
			json.Unmarshal(row.Tools, &def.Tools)
		}

		if len(row.StructuredOutput) > 0 {
			var so config.StructuredOutput
			if err := json.Unmarshal(row.StructuredOutput, &so); err == nil {
				def.StructuredOutput = &so
			}
		}

		defs = append(defs, def)
	}

	return defs
}

func (s *DBStore) GetRawDefinition(name string) ([]byte, error) {
	ctx := context.Background()

	var yamlData string
	var agentID string
	err := s.pool.QueryRow(ctx, `SELECT agent_id FROM agent_definitions WHERE name = $1`, name).Scan(&agentID)
	if err != nil {
		return nil, fmt.Errorf("definition %q not found", name)
	}

	err = s.pool.QueryRow(ctx,
		`SELECT definition_yaml FROM agent_versions WHERE agent_id = $1 ORDER BY created_at DESC LIMIT 1`, agentID).
		Scan(&yamlData)
	if err != nil {
		// Fall back to generating YAML from the definition row
		def := s.GetDefinition(name)
		if def == nil {
			return nil, fmt.Errorf("definition %q not found", name)
		}
		yamlBytes, err := yaml.Marshal(def)
		if err != nil {
			return nil, fmt.Errorf("marshal definition: %w", err)
		}
		return yamlBytes, nil
	}

	return []byte(yamlData), nil
}

func (s *DBStore) SaveRawDefinition(name string, data []byte) error {
	var def config.Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	if err := def.Validate(); err != nil {
		return err
	}
	return s.SaveDefinition(&def)
}

func (s *DBStore) resolveAgentID(ctx context.Context, name string) (string, error) {
	var agentID string
	err := s.pool.QueryRow(ctx, `SELECT agent_id FROM agent_definitions WHERE name = $1`, name).Scan(&agentID)
	return agentID, err
}

func (s *DBStore) ListVersions(ctx context.Context, name string) ([]config.AgentVersion, error) {
	agentID, err := s.resolveAgentID(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("definition %q not found", name)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, created_at FROM agent_versions WHERE agent_id = $1 ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("list versions for %s: %w", name, err)
	}
	defer rows.Close()

	var versions []config.AgentVersion
	for rows.Next() {
		var av config.AgentVersion
		var ts time.Time
		if err := rows.Scan(&av.VersionID, &ts); err != nil {
			continue
		}
		av.LastModified = ts.Format("2006-01-02T15:04:05Z")
		versions = append(versions, av)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(versions) > 0 {
		versions[0].IsLatest = true
	}

	return versions, nil
}

func (s *DBStore) GetVersion(ctx context.Context, name, versionID string) ([]byte, *config.Definition, error) {
	agentID, err := s.resolveAgentID(ctx, name)
	if err != nil {
		return nil, nil, fmt.Errorf("definition %q not found", name)
	}

	var yamlData string
	err = s.pool.QueryRow(ctx,
		`SELECT definition_yaml FROM agent_versions WHERE agent_id = $1 AND id = $2`, agentID, versionID).
		Scan(&yamlData)
	if err != nil {
		return nil, nil, fmt.Errorf("get version %s of %s: %w", versionID, name, err)
	}

	var def config.Definition
	if err := yaml.Unmarshal([]byte(yamlData), &def); err != nil {
		return []byte(yamlData), nil, fmt.Errorf("parse version: %w", err)
	}

	return []byte(yamlData), &def, nil
}

func (s *DBStore) Rollback(ctx context.Context, name, versionID string) error {
	_, parse, err := s.GetVersion(ctx, name, versionID)
	if err != nil {
		return err
	}

	return s.SaveDefinition(parse)
}

func generateAgentID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return fmt.Sprintf("%x", buf[:])
}
