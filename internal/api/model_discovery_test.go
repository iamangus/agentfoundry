package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angoo/agentfoundry/internal/store"
)

func TestModelDiscoveryProviderFormats(t *testing.T) {
	for _, typ := range []string{"openai", "openrouter", "deepseek", "ollama", "llama-server", "custom", "anthropic"} {
		t.Run(typ, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/models" || r.Header.Get("X-Custom") != "value" {
					t.Errorf("wrong path/headers: %s %v", r.URL, r.Header)
				}
				if typ == "anthropic" {
					if r.Header.Get("x-api-key") != "secret" || r.Header.Get("anthropic-version") == "" {
						t.Error("missing Anthropic authentication")
					}
					if calls == 1 {
						fmt.Fprint(w, `{"data":[{"id":"z","display_name":"Zed"}],"has_more":true,"last_id":"z"}`)
						return
					}
					if r.URL.Query().Get("after_id") != "z" {
						t.Error("missing pagination cursor")
					}
				} else if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("missing bearer authentication")
				}
				fmt.Fprint(w, `{"data":[{"id":"z","name":"Zed"},{"id":"a"},{"id":"a"},{"id":""}]}`)
			}))
			defer server.Close()
			models, err := fetchAvailableModels(httptest.NewRequest("GET", "/", nil), &store.ProviderRecord{ProviderType: typ, BaseURL: server.URL + "/v1/", APIKey: "secret", Headers: map[string]string{"X-Custom": "value"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 2 || models[0].ID != "a" || models[1].ID != "z" || models[1].Name != "Zed" {
				t.Fatalf("unexpected models: %+v", models)
			}
		})
	}
}

func TestModelDiscoveryFailuresAndEmptyList(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"unauthorized", 401, `secret-provider-response`, true},
		{"unsupported", 404, `no models`, true},
		{"bad-json", 200, `broken`, true},
		{"bad-format", 200, `{}`, true},
		{"empty", 200, `{"data":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			_, err := fetchAvailableModels(httptest.NewRequest("GET", "/", nil), &store.ProviderRecord{BaseURL: server.URL})
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-provider-response") {
				t.Fatal("upstream body leaked")
			}
		})
	}
	for _, base := range []string{"", "file:///etc/passwd", "http://user:secret@example.com", "https://example.com?api_key=secret"} {
		if _, err := fetchAvailableModels(httptest.NewRequest("GET", "/", nil), &store.ProviderRecord{BaseURL: base}); err == nil {
			t.Errorf("accepted %q", base)
		}
	}
}

func TestModelDiscoveryDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect with credentials") }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer server.Close()
	if _, err := fetchAvailableModels(httptest.NewRequest("GET", "/", nil), &store.ProviderRecord{BaseURL: server.URL, APIKey: "secret"}); err == nil {
		t.Fatal("accepted redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchAvailableModels(httptest.NewRequest("GET", "/", nil).WithContext(ctx), &store.ProviderRecord{BaseURL: server.URL}); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestNewInferenceEndpointsRequireAuthentication(t *testing.T) {
	h := &Handler{}
	for _, handler := range []http.HandlerFunc{h.listInferenceProfiles, h.saveInferenceProfile, h.deleteInferenceProfile, h.discoverModels, h.listProviderModels, h.getModelCapabilities} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("POST", "/", strings.NewReader(`{}`)))
		if w.Code != 401 {
			t.Fatalf("expected unauthorized, got %d", w.Code)
		}
	}
	if providerBaseURL("deepseek", "") != "https://api.deepseek.com" {
		t.Fatal("missing DeepSeek defaults")
	}
}
