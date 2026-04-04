package auth_test

import (
	"testing"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/config"
)

func TestAuthContext_HasRole(t *testing.T) {
	ac := &auth.AuthContext{
		Roles: []string{"opendev-user", "team-admin"},
	}

	if !ac.HasRole("opendev-user") {
		t.Error("expected HasRole to return true for opendev-user")
	}
	if !ac.HasRole("team-admin") {
		t.Error("expected HasRole to return true for team-admin")
	}
	if ac.HasRole("opendev-admin") {
		t.Error("expected HasRole to return false for opendev-admin")
	}
	if ac.HasRole("") {
		t.Error("expected HasRole to return false for empty role")
	}
}

func TestAuthContext_IsMemberOfTeam(t *testing.T) {
	ac := &auth.AuthContext{
		Teams: []string{"engineering", "product"},
	}

	if !ac.IsMemberOfTeam("engineering") {
		t.Error("expected IsMemberOfTeam to return true for engineering")
	}
	if !ac.IsMemberOfTeam("product") {
		t.Error("expected IsMemberOfTeam to return true for product")
	}
	if ac.IsMemberOfTeam("marketing") {
		t.Error("expected IsMemberOfTeam to return false for marketing")
	}
}

func TestAuthContext_CanManageTeamAgent(t *testing.T) {
	tests := []struct {
		name      string
		ac        *auth.AuthContext
		team      string
		canManage bool
	}{
		{
			name: "global admin can manage any team",
			ac: &auth.AuthContext{
				IsGlobalAdmin: true,
				Teams:         []string{},
			},
			team:      "engineering",
			canManage: true,
		},
		{
			name: "team admin in the team can manage",
			ac: &auth.AuthContext{
				IsTeamAdmin: true,
				Teams:       []string{"engineering"},
			},
			team:      "engineering",
			canManage: true,
		},
		{
			name: "team admin not in the team cannot manage",
			ac: &auth.AuthContext{
				IsTeamAdmin: true,
				Teams:       []string{"product"},
			},
			team:      "engineering",
			canManage: false,
		},
		{
			name: "regular team member cannot manage",
			ac: &auth.AuthContext{
				Teams: []string{"engineering"},
			},
			team:      "engineering",
			canManage: false,
		},
		{
			name: "user not in any team cannot manage",
			ac: &auth.AuthContext{
				Teams: []string{},
			},
			team:      "engineering",
			canManage: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ac.CanManageTeamAgent(tt.team)
			if got != tt.canManage {
				t.Errorf("CanManageTeamAgent() = %v, want %v", got, tt.canManage)
			}
		})
	}
}

func TestDefinition_VisibleTo(t *testing.T) {
	tests := []struct {
		name    string
		def     config.Definition
		subject string
		teams   []string
		isAdmin bool
		visible bool
	}{
		{
			name:    "global agent visible to everyone",
			def:     config.Definition{Scope: "global"},
			visible: true,
		},
		{
			name:    "global agent visible to admin",
			def:     config.Definition{Scope: "global"},
			isAdmin: true,
			visible: true,
		},
		{
			name:    "empty scope visible to everyone",
			def:     config.Definition{Scope: ""},
			visible: true,
		},
		{
			name:    "team agent visible to team member",
			def:     config.Definition{Scope: "team", Team: "engineering"},
			teams:   []string{"engineering"},
			visible: true,
		},
		{
			name:    "team agent not visible to non-member",
			def:     config.Definition{Scope: "team", Team: "engineering"},
			teams:   []string{"marketing"},
			visible: false,
		},
		{
			name:    "team agent visible to admin regardless of team",
			def:     config.Definition{Scope: "team", Team: "engineering"},
			isAdmin: true,
			visible: true,
		},
		{
			name:    "user agent visible to creator",
			def:     config.Definition{Scope: "user", CreatedBy: "user-1"},
			subject: "user-1",
			visible: true,
		},
		{
			name:    "user agent not visible to other user",
			def:     config.Definition{Scope: "user", CreatedBy: "user-1"},
			subject: "user-2",
			visible: false,
		},
		{
			name:    "user agent visible to admin",
			def:     config.Definition{Scope: "user", CreatedBy: "user-1"},
			isAdmin: true,
			visible: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.def.VisibleTo(tt.subject, tt.teams, tt.isAdmin)
			if got != tt.visible {
				t.Errorf("VisibleTo() = %v, want %v", got, tt.visible)
			}
		})
	}
}

func TestDefinition_CanEdit(t *testing.T) {
	tests := []struct {
		name    string
		def     config.Definition
		subject string
		teams   []string
		isAdmin bool
		isTA    bool
		canEdit bool
	}{
		{
			name:    "global agent only editable by admin",
			def:     config.Definition{Scope: "global"},
			isAdmin: true,
			canEdit: true,
		},
		{
			name:    "global agent not editable by non-admin",
			def:     config.Definition{Scope: "global"},
			canEdit: false,
		},
		{
			name:    "team agent editable by creator",
			def:     config.Definition{Scope: "team", Team: "eng", CreatedBy: "u1"},
			subject: "u1",
			teams:   []string{"eng"},
			canEdit: true,
		},
		{
			name:    "team agent editable by team admin",
			def:     config.Definition{Scope: "team", Team: "eng", CreatedBy: "u2"},
			subject: "u1",
			teams:   []string{"eng"},
			isTA:    true,
			canEdit: true,
		},
		{
			name:    "team agent not editable by non-team user",
			def:     config.Definition{Scope: "team", Team: "eng", CreatedBy: "u2"},
			subject: "u1",
			teams:   []string{"marketing"},
			canEdit: false,
		},
		{
			name:    "user agent editable by creator",
			def:     config.Definition{Scope: "user", CreatedBy: "u1"},
			subject: "u1",
			canEdit: true,
		},
		{
			name:    "user agent not editable by other user",
			def:     config.Definition{Scope: "user", CreatedBy: "u1"},
			subject: "u2",
			canEdit: false,
		},
		{
			name:    "empty scope editable by creator",
			def:     config.Definition{Scope: "", CreatedBy: "u1"},
			subject: "u1",
			canEdit: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.def.CanEdit(tt.subject, tt.teams, tt.isAdmin, tt.isTA)
			if got != tt.canEdit {
				t.Errorf("CanEdit() = %v, want %v", got, tt.canEdit)
			}
		})
	}
}

func TestDefinition_CanDelete(t *testing.T) {
	tests := []struct {
		name      string
		def       config.Definition
		subject   string
		teams     []string
		isAdmin   bool
		isTA      bool
		canDelete bool
	}{
		{
			name:      "global agent only deletable by admin",
			def:       config.Definition{Scope: "global"},
			isAdmin:   true,
			canDelete: true,
		},
		{
			name:      "global agent not deletable by non-admin",
			def:       config.Definition{Scope: "global"},
			canDelete: false,
		},
		{
			name:      "team agent deletable by creator",
			def:       config.Definition{Scope: "team", Team: "eng", CreatedBy: "u1"},
			subject:   "u1",
			teams:     []string{"eng"},
			canDelete: true,
		},
		{
			name:      "team agent deletable by team admin",
			def:       config.Definition{Scope: "team", Team: "eng", CreatedBy: "u2"},
			subject:   "u1",
			teams:     []string{"eng"},
			isTA:      true,
			canDelete: true,
		},
		{
			name:      "team agent not deletable by regular member",
			def:       config.Definition{Scope: "team", Team: "eng", CreatedBy: "u2"},
			subject:   "u1",
			teams:     []string{"eng"},
			canDelete: false,
		},
		{
			name:      "user agent deletable by creator",
			def:       config.Definition{Scope: "user", CreatedBy: "u1"},
			subject:   "u1",
			canDelete: true,
		},
		{
			name:      "user agent not deletable by other",
			def:       config.Definition{Scope: "user", CreatedBy: "u1"},
			subject:   "u2",
			canDelete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.def.CanDelete(tt.subject, tt.teams, tt.isAdmin, tt.isTA)
			if got != tt.canDelete {
				t.Errorf("CanDelete() = %v, want %v", got, tt.canDelete)
			}
		})
	}
}

func TestExtractNestedClaim(t *testing.T) {
	claims := map[string]any{
		"realm_access": map[string]any{
			"roles": []any{"opendev-user", "team-admin"},
		},
		"groups": []any{"/engineering", "/product"},
		"simple": "single-value",
	}

	roles := auth.ExtractNestedClaim(claims, "realm_access.roles")
	if len(roles) != 2 || roles[0] != "opendev-user" || roles[1] != "team-admin" {
		t.Errorf("got roles %v, want [opendev-user team-admin]", roles)
	}

	groups := auth.ExtractNestedClaim(claims, "groups")
	if len(groups) != 2 || groups[0] != "/engineering" || groups[1] != "/product" {
		t.Errorf("got groups %v, want [/engineering /product]", groups)
	}

	simple := auth.ExtractNestedClaim(claims, "simple")
	if len(simple) != 1 || simple[0] != "single-value" {
		t.Errorf("got simple %v, want [single-value]", simple)
	}

	missing := auth.ExtractNestedClaim(claims, "nonexistent.path")
	if missing != nil {
		t.Errorf("expected nil for missing path, got %v", missing)
	}

	badType := auth.ExtractNestedClaim(claims, "realm_access.roles.extra")
	if badType != nil {
		t.Errorf("expected nil for non-array leaf, got %v", badType)
	}
}

func TestConfig_LoadDefaults(t *testing.T) {
	t.Setenv("AUTH_ISSUER", "https://keycloak.example.com/realms/test")

	cfg := auth.LoadConfig()

	if !cfg.Enabled() {
		t.Error("expected config to be enabled")
	}
	if cfg.Issuer != "https://keycloak.example.com/realms/test" {
		t.Errorf("got issuer %q", cfg.Issuer)
	}
	if cfg.RolesClaim != "realm_access.roles" {
		t.Errorf("got roles claim %q", cfg.RolesClaim)
	}
	if cfg.GroupsClaim != "groups" {
		t.Errorf("got groups claim %q", cfg.GroupsClaim)
	}
	if len(cfg.AdminRoles) != 1 || cfg.AdminRoles[0] != "opendev-admin" {
		t.Errorf("got admin roles %v", cfg.AdminRoles)
	}
	if cfg.TeamAdminRole != "team-admin" {
		t.Errorf("got team admin role %q", cfg.TeamAdminRole)
	}
	if len(cfg.AccessRoles) != 1 || cfg.AccessRoles[0] != "opendev-user" {
		t.Errorf("got access roles %v", cfg.AccessRoles)
	}
}

func TestConfig_DisabledWhenNoIssuer(t *testing.T) {
	cfg := auth.LoadConfig()
	if cfg.Enabled() {
		t.Error("expected config to be disabled without AUTH_ISSUER")
	}
}
