package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/angoo/agentfoundry/internal/auth"
)

func TestProviderHealthUsesAuthenticationMiddleware(t *testing.T) {
	middleware := auth.NewMiddleware(nil, nil, nil, &auth.Config{Issuer: "enabled", InternalAPIKey: "test-key"})
	handler := middleware.Handler(authExemptPaths...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.FromContext(r) == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		path, key string
		status    int
	}{
		{"/api/v1/inference/providers", "", http.StatusUnauthorized},
		{"/api/v1/inference/providers", "test-key", http.StatusNoContent},
		{"/api/v1/runs/run/inference", "test-key", http.StatusNoContent},
		{"/api/v1/inference/agents/agent/chat/completions", "", http.StatusAccepted},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		if tc.key != "" {
			r.Header.Set("Authorization", "Bearer "+tc.key)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s: got %d, want %d", tc.path, w.Code, tc.status)
		}
	}
}
