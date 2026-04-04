package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type groupCacheEntry struct {
	groups []string
	roles  []string
	expiry time.Time
}

type GroupCache struct {
	issuer        string
	realm         string
	adminClientID string
	adminSecret   string
	mu            sync.RWMutex
	cache         map[string]*groupCacheEntry
	ttl           time.Duration
	client        *http.Client
}

func NewGroupCache(issuer, realm, adminClientID, adminSecret string) *GroupCache {
	return &GroupCache{
		issuer:        issuer,
		realm:         realm,
		adminClientID: adminClientID,
		adminSecret:   adminSecret,
		cache:         make(map[string]*groupCacheEntry),
		ttl:           60 * time.Second,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *GroupCache) GetUserGroups(ctx context.Context, subject string) ([]string, error) {
	entry := c.get(subject)
	if entry != nil {
		return entry.groups, nil
	}

	groups, roles, err := c.fetchUser(ctx, subject)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cache[subject] = &groupCacheEntry{
		groups: groups,
		roles:  roles,
		expiry: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()

	return groups, nil
}

func (c *GroupCache) GetUserRoles(ctx context.Context, subject string) ([]string, error) {
	entry := c.get(subject)
	if entry != nil {
		return entry.roles, nil
	}

	groups, roles, err := c.fetchUser(ctx, subject)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cache[subject] = &groupCacheEntry{
		groups: groups,
		roles:  roles,
		expiry: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()

	return roles, nil
}

func (c *GroupCache) get(subject string) *groupCacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.cache[subject]
	if !ok || time.Now().After(entry.expiry) {
		return nil
	}
	return entry
}

type keycloakUser struct {
	ID         string   `json:"id"`
	Username   string   `json:"username"`
	RealmRoles []string `json:"realmRoles"`
	Groups     []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"groups"`
}

func (c *GroupCache) exchangeAdminToken(ctx context.Context) (string, error) {
	tokenURL := c.issuer + "/protocol/openid-connect/token"
	data := "grant_type=client_credentials" +
		"&client_id=" + c.adminClientID +
		"&client_secret=" + c.adminSecret

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak token exchange: %s", resp.Status)
	}

	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}

	return tr.AccessToken, nil
}

func (c *GroupCache) fetchUser(ctx context.Context, subject string) ([]string, []string, error) {
	adminToken, err := c.exchangeAdminToken(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("exchange admin token: %w", err)
	}

	url := c.issuer + "/admin/realms/" + c.realm + "/users/" + subject
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("keycloak user lookup: %s", resp.Status)
	}

	var user keycloakUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, nil, fmt.Errorf("decode user: %w", err)
	}

	groups := make([]string, 0, len(user.Groups))
	for _, g := range user.Groups {
		name := strings.TrimPrefix(g.Path, "/")
		if name != "" && !strings.Contains(name, "/") {
			groups = append(groups, name)
		}
	}

	return groups, user.RealmRoles, nil
}

func (c *GroupCache) Invalidate(subject string) {
	c.mu.Lock()
	delete(c.cache, subject)
	c.mu.Unlock()
}
