package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/run"
	"github.com/angoo/agentfoundry/internal/store"
)

type memoryInference struct {
	records                []store.InferenceAttempt
	runID, inputID, cursor string
}

func (s *memoryInference) Save(_ context.Context, a store.InferenceAttempt) error {
	s.records = append(s.records, a)
	return nil
}
func (s *memoryInference) List(_ context.Context, runID, inputID, cursor string) (store.InferencePage, error) {
	s.runID, s.inputID, s.cursor = runID, inputID, cursor
	return store.InferencePage{Attempts: s.records}, nil
}

func TestInferenceObserverUsageAndTimings(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	o := newInferenceObserver(r, "agent", "provider", "model", false)
	o.started = time.Now().Add(-time.Second)
	o.observe([]byte(`{"choices":[{"delta":{"role":"assistant"}}]}`), true)
	if !o.firstToken.IsZero() {
		t.Fatal("role delta counted as token")
	}
	o.observe([]byte(`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`), true)
	if o.firstToken.IsZero() || !o.firstVisible.IsZero() {
		t.Fatal("reasoning timing incorrect")
	}
	o.firstToken = time.Now().Add(-time.Second)
	o.observe([]byte(`{"choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":11,"prompt_tokens_details":{"cached_tokens":80}}}`), true)
	o.record.HTTPStatus = 200
	s := &memoryInference{}
	h := &Handler{inferenceStore: s}
	h.finishInference(r, o)
	m := s.records[0].Metrics
	if o.record.Status != "completed" || m.CacheHitRatio == nil || *m.CacheHitRatio != .8 || m.TimeToFirstTokenMS == nil || m.TimeToFirstVisibleTokenMS == nil || m.ObservedOutputTokensPerSecond == nil {
		t.Fatalf("missing metrics: %+v", o.record)
	}
	if m.PrefillMS != nil || m.TimeToStartPrefillMS != nil {
		t.Fatal("invented provider timings")
	}
}

func TestInferenceObserverIdentity(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-AgentFoundry-Execution-ID", "execution")
	r.Header.Set("X-AgentFoundry-Activity-ID", "activity")
	one := newInferenceObserver(r, "a", "p", "m", false)
	two := newInferenceObserver(r, "a", "p", "m", false)
	if one.record.ID != two.record.ID {
		t.Fatal("identity not idempotent")
	}
	r.Header.Set("X-AgentFoundry-Activity-Attempt", "2")
	if one.record.ID == newInferenceObserver(r, "a", "p", "m", false).record.ID {
		t.Fatal("retry identity collision")
	}
}

func TestInferenceLargeResponseAndProviderError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	o := newInferenceObserver(r, "a", "p", "m", false)
	if err := copyInferenceResponse(io.Discard, strings.NewReader(strings.Repeat("x", 2<<20)), o); err != nil || !o.finished {
		t.Fatal("capture limit affected successful delivery")
	}
	o.observe([]byte(`{"error":{"message":"failed"}}`), true)
	o.observe([]byte(`[DONE]`), true)
	o.record.HTTPStatus = 200
	h := &Handler{inferenceStore: &memoryInference{}}
	h.finishInference(r, o)
	if o.record.Status != "provider_error" {
		t.Fatal("provider error marked completed")
	}
}

func TestRunInferenceAuthorizationAndFilter(t *testing.T) {
	runs := run.New()
	ru := runs.Create("agent", "owner", "", "")
	s := &memoryInference{}
	h := &Handler{runs: runs, inferenceStore: s}
	for _, tc := range []struct {
		subject string
		status  int
	}{{"", 401}, {"other", 404}, {"owner", 200}} {
		r := httptest.NewRequest(http.MethodGet, "/?input_id=turn&cursor=next", nil)
		r.SetPathValue("id", ru.ID)
		if tc.subject != "" {
			r = r.WithContext(auth.NewContext(r.Context(), &auth.AuthContext{Subject: tc.subject}))
		}
		w := httptest.NewRecorder()
		h.getRunInference(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: %d", tc.subject, w.Code)
		}
	}
	if s.runID != ru.ID || s.inputID != "turn" || s.cursor != "next" {
		t.Fatal("lost turn filter")
	}
}

func TestProviderMetricsParserAndSampling(t *testing.T) {
	text := `sglang:num_queue_reqs{model_name="m"} 2
sglang:queue_time_seconds_sum 4
sglang:queue_time_seconds_count 2
sglang:prompt_tokens_total 100
sglang:cached_tokens_total{source="device"} 60
sglang:cached_tokens_total{source="host"} 20
sglang:realtime_tokens_total{mode="prefill_compute"} 100
sglang:realtime_tokens_total{mode="prefill_cache"} 80
sglang:realtime_tokens_total{mode="decode"} 10
unrelated_secret 999
`
	v, err := parseProviderMetrics(text)
	if err != nil || v["cached_tokens_total"] != 80 || v["realtime_tokens_total/decode"] != 10 {
		t.Fatalf("bad parsing: %v %v", v, err)
	}
	if _, ok := v["unrelated_secret"]; ok {
		t.Fatal("unexpected metric leaked")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			t.Error(r.URL.Path)
		}
		io.WriteString(w, text)
	}))
	defer server.Close()
	h := &Handler{}
	p := store.ProviderRecord{ID: "provider", ProviderType: "custom", BaseURL: server.URL + "/v1"}
	first := h.sampleProvider(context.Background(), p)
	if !first.Available || first.CacheHitRatio == nil || *first.CacheHitRatio != .8 || first.QueueMeanMS == nil || *first.QueueMeanMS != 2000 || first.OutputTokensPerSecond != nil {
		t.Fatalf("bad first sample %+v", first)
	}
	h.providerMetrics.previous[p.ID] = providerCounters{at: time.Now().Add(-10 * time.Second), compute: 50, cache: 20, decode: 0, ready: true}
	next := h.sampleProvider(context.Background(), p)
	if next.OutputTokensPerSecond == nil || *next.OutputTokensPerSecond < .9 || *next.OutputTokensPerSecond > 1.1 {
		t.Fatalf("bad rate %+v", next)
	}
}

func TestProviderMetricsRefusesRedirectsAndOversizedResponses(t *testing.T) {
	for _, kind := range []string{"redirect", "oversized", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "redirect" {
					http.Redirect(w, r, "http://127.0.0.1:1/credentials", http.StatusFound)
					return
				}
				if kind == "oversized" {
					io.WriteString(w, strings.Repeat("x", (2<<20)+1))
					return
				}
				io.WriteString(w, "unrelated_metric 1\n")
			}))
			defer server.Close()
			h := &Handler{}
			result := h.sampleProvider(context.Background(), store.ProviderRecord{ProviderType: "custom", BaseURL: server.URL + "/v1", APIKey: "secret"})
			if result.Available || result.Message == "" || strings.Contains(result.Message, "secret") {
				t.Fatalf("invalid health result: %+v", result)
			}
		})
	}
}

func TestInferenceMissingUsageAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	o := newInferenceObserver(r, "a", "p", "m", false)
	o.observe([]byte(`{"choices":[{"delta":{"content":"hello"},"finish_reason":"stop"}]}`), true)
	o.record.HTTPStatus = 200
	cancel()
	s := &memoryInference{}
	h := &Handler{inferenceStore: s}
	h.finishInference(r, o)
	if len(s.records) != 1 || o.record.Status != "canceled" || o.record.Metrics.InputTokens != nil || o.record.Metrics.CacheHitRatio != nil || o.record.Metrics.ObservedOutputTokensPerSecond != nil {
		t.Fatalf("incorrect missing/cancellation semantics: %+v", o.record)
	}
}
