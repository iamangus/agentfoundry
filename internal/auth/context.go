package auth

import (
	"context"
	"net/http"
)

type AuthContext struct {
	Subject       string
	Username      string
	Email         string
	AuthMethod    string
	Roles         []string
	Groups        []string
	IsGlobalAdmin bool
	IsTeamAdmin   bool
	Teams         []string
	APIKeyName    string
}

type contextKey struct{}

func FromContext(r *http.Request) *AuthContext {
	if c, ok := r.Context().Value(contextKey{}).(*AuthContext); ok {
		return c
	}
	return nil
}

func NewContext(ctx context.Context, ac *AuthContext) context.Context {
	return context.WithValue(ctx, contextKey{}, ac)
}

func (a *AuthContext) HasRole(role string) bool {
	for _, r := range a.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func (a *AuthContext) IsMemberOfTeam(team string) bool {
	for _, t := range a.Teams {
		if t == team {
			return true
		}
	}
	return false
}

func (a *AuthContext) CanManageTeamAgent(team string) bool {
	if a.IsGlobalAdmin {
		return true
	}
	if !a.IsMemberOfTeam(team) {
		return false
	}
	if a.IsTeamAdmin {
		return true
	}
	return false
}
