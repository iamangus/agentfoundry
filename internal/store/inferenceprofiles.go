package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// InferenceProfile is a routing template, never a live reference in an agent.
// Models are resolved to explicit IDs when saving a profile so that changing a
// provider's default does not silently change what the template applies.
type InferenceProfile struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	ProviderID       string    `json:"provider_id"`
	Model            string    `json:"model"`
	BackupProviderID string    `json:"backup_provider_id"`
	BackupModel      string    `json:"backup_model"`
	Scope            string    `json:"scope"`
	Team             string    `json:"team"`
	CreatedBy        string    `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

const profileColumns = `id, name, provider_id, model, COALESCE(backup_provider_id, ''), backup_model, scope, team, created_by, created_at, updated_at`

func scanProfile(row pgx.Row) (*InferenceProfile, error) {
	p := &InferenceProfile{}
	err := row.Scan(&p.ID, &p.Name, &p.ProviderID, &p.Model, &p.BackupProviderID, &p.BackupModel, &p.Scope, &p.Team, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (s *ProviderStore) ListProfiles(ctx context.Context, subject string, teams []string, admin bool) ([]InferenceProfile, error) {
	rows, err := s.db.Query(ctx, `SELECT `+profileColumns+` FROM inference_profiles ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []InferenceProfile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		if s.visibleTo(p.Scope, p.Team, p.CreatedBy, subject, teams, admin) || admin {
			result = append(result, *p)
		}
	}
	return result, rows.Err()
}

func (s *ProviderStore) GetProfile(ctx context.Context, id string) (*InferenceProfile, error) {
	return scanProfile(s.db.QueryRow(ctx, `SELECT `+profileColumns+` FROM inference_profiles WHERE id=$1`, id))
}

func (s *ProviderStore) SaveProfile(ctx context.Context, p *InferenceProfile) (*InferenceProfile, error) {
	if p.ID == "" {
		return scanProfile(s.db.QueryRow(ctx, `INSERT INTO inference_profiles (name, provider_id, model, backup_provider_id, backup_model, scope, team, created_by) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8) RETURNING `+profileColumns, p.Name, p.ProviderID, p.Model, p.BackupProviderID, p.BackupModel, p.Scope, p.Team, p.CreatedBy))
	}
	return scanProfile(s.db.QueryRow(ctx, `UPDATE inference_profiles SET name=$2, provider_id=$3, model=$4, backup_provider_id=NULLIF($5,''), backup_model=$6, scope=$7, team=$8, updated_at=NOW() WHERE id=$1 RETURNING `+profileColumns, p.ID, p.Name, p.ProviderID, p.Model, p.BackupProviderID, p.BackupModel, p.Scope, p.Team))
}

func (s *ProviderStore) DeleteProfile(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM inference_profiles WHERE id=$1`, id)
	return err
}
