package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrKeyNotFound = errors.New("api key not found")
var ErrKeyRevoked = errors.New("api key has been revoked")
var ErrKeyExpired = errors.New("api key has expired")

const keyPrefix = "afk_"
const keyBytes = 32

type APIKeyRecord struct {
	ID           string
	Name         string
	KeyPrefix    string
	OwnerSubject string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
}

type APIKeyStore struct {
	db *pgxpool.Pool
}

func NewAPIKeyStore(db *pgxpool.Pool) *APIKeyStore {
	return &APIKeyStore{db: db}
}

func (s *APIKeyStore) Create(ctx context.Context, name string, ownerSubject string, expiresAt *time.Time) (id, prefix, fullKey string, err error) {
	rawBytes := make([]byte, keyBytes)
	if _, err := rand.Read(rawBytes); err != nil {
		return "", "", "", fmt.Errorf("generate key: %w", err)
	}
	fullKey = keyPrefix + hex.EncodeToString(rawBytes)
	prefix = fullKey[:len(keyPrefix)+8]

	hash := sha256.Sum256([]byte(fullKey))
	hashStr := hex.EncodeToString(hash[:])

	err = s.db.QueryRow(ctx, `INSERT INTO api_keys (name, key_hash, key_prefix, owner_subject, expires_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		name, hashStr, prefix, ownerSubject, expiresAt,
	).Scan(&id)
	if err != nil {
		return "", "", "", fmt.Errorf("insert api key: %w", err)
	}

	return id, prefix, fullKey, nil
}

func (s *APIKeyStore) Validate(ctx context.Context, fullKey string) (*APIKeyRecord, error) {
	hash := sha256.Sum256([]byte(fullKey))
	hashStr := hex.EncodeToString(hash[:])

	var rec APIKeyRecord
	var lastUsed, expiresAt, revokedAt *time.Time

	err := s.db.QueryRow(ctx,
		`SELECT id, name, key_prefix, owner_subject, created_at, last_used_at, expires_at, revoked_at FROM api_keys WHERE key_hash = $1`,
		hashStr,
	).Scan(&rec.ID, &rec.Name, &rec.KeyPrefix, &rec.OwnerSubject,
		&rec.CreatedAt, &lastUsed, &expiresAt, &revokedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup api key: %w", err)
	}

	if revokedAt != nil {
		return nil, ErrKeyRevoked
	}

	if expiresAt != nil && time.Now().After(*expiresAt) {
		return nil, ErrKeyExpired
	}

	rec.LastUsedAt = lastUsed
	rec.ExpiresAt = expiresAt

	go func() {
		updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.db.Exec(updateCtx, `UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`, rec.ID)
	}()

	return &rec, nil
}

func (s *APIKeyStore) List(ctx context.Context, ownerSubject string) ([]APIKeyRecord, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, name, key_prefix, created_at, last_used_at, expires_at, revoked_at FROM api_keys WHERE owner_subject = $1 ORDER BY created_at DESC`,
		ownerSubject,
	)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKeyRecord
	for rows.Next() {
		var rec APIKeyRecord
		var lastUsed, expiresAt, revokedAt *time.Time
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.KeyPrefix, &rec.CreatedAt, &lastUsed, &expiresAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		rec.LastUsedAt = lastUsed
		rec.ExpiresAt = expiresAt
		keys = append(keys, rec)
	}

	return keys, nil
}

func (s *APIKeyStore) Revoke(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `UPDATE api_keys SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}
