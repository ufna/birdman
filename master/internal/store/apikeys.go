package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// ErrBadAPIKey is returned when API key authentication fails.
var ErrBadAPIKey = errors.New("bad_api_key")

type APIKey struct {
	ID     string
	Name   string
	Scopes []string
}

// CountActiveAPIKeys — used by startup bootstrap.
func (s *Store) CountActiveAPIKeys(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`select count(*) from api_keys where revoked_at is null`).Scan(&n)
	return n, err
}

// CreateAPIKey mints a key with the given scopes and returns the bearer
// secret — shown exactly once.
func (s *Store) CreateAPIKey(ctx context.Context, name string, scopes []string) (APIKey, string, error) {
	if name == "" || len(scopes) == 0 {
		return APIKey{}, "", fmt.Errorf("name and scopes are required")
	}
	secret, err := newSecret()
	if err != nil {
		return APIKey{}, "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return APIKey{}, "", err
	}
	var k APIKey
	err = s.Pool.QueryRow(ctx, `
		insert into api_keys (name, hash, scopes)
		values ($1, $2, $3)
		returning id::text, name, scopes`, name, string(hash), scopes).
		Scan(&k.ID, &k.Name, &k.Scopes)
	if err != nil {
		return APIKey{}, "", err
	}
	return k, composeToken(apiKeyPrefix, k.ID, secret), nil
}

// AuthAPIKey verifies a bearer key and returns its scopes.
func (s *Store) AuthAPIKey(ctx context.Context, token string) (APIKey, error) {
	id, secret, err := parseToken(apiKeyPrefix, token)
	if err != nil {
		return APIKey{}, ErrBadAPIKey
	}
	var k APIKey
	var hash string
	err = s.Pool.QueryRow(ctx, `
		select id::text, name, scopes, hash from api_keys
		where id = $1::uuid and revoked_at is null`, id).
		Scan(&k.ID, &k.Name, &k.Scopes, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrBadAPIKey
	}
	if err != nil {
		return APIKey{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) != nil {
		return APIKey{}, ErrBadAPIKey
	}
	return k, nil
}
