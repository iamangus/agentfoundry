package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/store"
)

type providerCounters struct {
	at                     time.Time
	compute, cache, decode float64
	ready                  bool
}
type providerMetricCache struct {
	sync.Mutex
	previous map[string]providerCounters
}
type providerHealth struct {
	ID                     string    `json:"id"`
	Name                   string    `json:"name"`
	ProviderType           string    `json:"provider_type"`
	Scope                  string    `json:"scope"`
	Available              bool      `json:"available"`
	Message                string    `json:"message,omitempty"`
	SampledAt              time.Time `json:"sampled_at"`
	QueueDepth             *float64  `json:"queue_depth,omitempty"`
	QueueMeanMS            *float64  `json:"queue_mean_ms,omitempty"`
	TTFTMeanMS             *float64  `json:"ttft_mean_ms,omitempty"`
	CacheHitRatio          *float64  `json:"cache_hit_ratio,omitempty"`
	PrefillTokensPerSecond *float64  `json:"prefill_tokens_per_second,omitempty"`
	OutputTokensPerSecond  *float64  `json:"output_tokens_per_second,omitempty"`
	RateWindowSeconds      *float64  `json:"rate_window_seconds,omitempty"`
	Source                 string    `json:"source,omitempty"`
}

var metricLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})?\s+([^\s]+)`)
var metricMode = regexp.MustCompile(`(?:\{|,)\s*mode="([^"]+)"`)

func parseProviderMetrics(text string) (map[string]float64, error) {
	values := make(map[string]float64)
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 4096), 128*1024)
	allowed := map[string]bool{"num_queue_reqs": true, "queue_time_seconds_sum": true, "queue_time_seconds_count": true, "time_to_first_token_seconds_sum": true, "time_to_first_token_seconds_count": true, "prompt_tokens_total": true, "cached_tokens_total": true, "realtime_tokens_total": true}
	for scanner.Scan() {
		line := metricLine.FindStringSubmatch(scanner.Text())
		if line == nil {
			continue
		}
		name := strings.TrimPrefix(line[1], "sglang:")
		if name == line[1] || !allowed[name] {
			continue
		}
		n, err := strconv.ParseFloat(line[3], 64)
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			continue
		}
		if name == "realtime_tokens_total" {
			mode := metricMode.FindStringSubmatch(line[2])
			if len(mode) != 2 {
				continue
			}
			name += "/" + mode[1]
		}
		values[name] += n
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("no supported metrics")
	}
	return values, nil
}

func (h *Handler) getProviderInference(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	providers, err := h.providerStore.List(r.Context(), ac.Subject, ac.Teams, ac.IsGlobalAdmin)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not list providers"})
		return
	}
	id := r.URL.Query().Get("provider_id")
	if id == "" {
		list := []providerHealth{}
		for _, p := range providers {
			list = append(list, providerHealth{ID: p.ID, Name: p.Name, ProviderType: p.ProviderType, Scope: "server_aggregate"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"providers": list})
		return
	}
	for _, p := range providers {
		if p.ID == id {
			writeJSON(w, http.StatusOK, h.sampleProvider(r.Context(), p))
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider not found"})
}

func (h *Handler) sampleProvider(ctx context.Context, p store.ProviderRecord) providerHealth {
	result := providerHealth{ID: p.ID, Name: p.Name, ProviderType: p.ProviderType, Scope: "server_aggregate", SampledAt: time.Now().UTC()}
	if p.ProviderType != "custom" && p.ProviderType != "sglang" {
		result.Message = "This provider does not expose a supported server metrics endpoint."
		return result
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		result.Message = "Invalid provider endpoint."
		return result
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1") + "/metrics"
	u.RawQuery = ""
	u.Fragment = ""
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		result.Message = "Metrics endpoint unavailable."
		return result
	}
	for key, value := range p.Headers {
		req.Header.Set(key, value)
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		result.Message = "Could not reach the provider metrics endpoint."
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		result.Message = "Provider does not expose readable metrics (HTTP " + strconv.Itoa(resp.StatusCode) + ")."
		return result
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(body) > 2*1024*1024 {
		result.Message = "Provider metrics response exceeded the read limit."
		return result
	}
	values, err := parseProviderMetrics(string(body))
	if err != nil {
		result.Message = "No supported SGLang metrics were reported."
		return result
	}
	result.Available = true
	result.Source = "sglang_prometheus"
	if n, ok := values["num_queue_reqs"]; ok {
		result.QueueDepth = &n
	}
	mean := func(sum, count string) *float64 {
		s, sok := values[sum]
		c, cok := values[count]
		if !sok || !cok || c <= 0 {
			return nil
		}
		n := s / c * 1000
		return &n
	}
	result.QueueMeanMS = mean("queue_time_seconds_sum", "queue_time_seconds_count")
	result.TTFTMeanMS = mean("time_to_first_token_seconds_sum", "time_to_first_token_seconds_count")
	if prompt, ok := values["prompt_tokens_total"]; ok && prompt > 0 {
		if cached, ok := values["cached_tokens_total"]; ok && cached <= prompt {
			n := cached / prompt
			result.CacheHitRatio = &n
		}
	}
	compute, cok := values["realtime_tokens_total/prefill_compute"]
	cache, kok := values["realtime_tokens_total/prefill_cache"]
	decode, dok := values["realtime_tokens_total/decode"]
	next := providerCounters{at: time.Now(), compute: compute, cache: cache, decode: decode, ready: cok && kok && dok}
	h.providerMetrics.Lock()
	defer h.providerMetrics.Unlock()
	if h.providerMetrics.previous == nil {
		h.providerMetrics.previous = make(map[string]providerCounters)
	}
	previous := h.providerMetrics.previous[p.ID]
	if previous.ready && next.ready {
		seconds := next.at.Sub(previous.at).Seconds()
		if seconds >= 1 && seconds <= 120 && compute >= previous.compute && cache >= previous.cache && decode >= previous.decode {
			prefill := (compute - previous.compute) / seconds
			output := (decode - previous.decode) / seconds
			result.PrefillTokensPerSecond = &prefill
			result.OutputTokensPerSecond = &output
			result.RateWindowSeconds = &seconds
		}
	}
	if previous.at.IsZero() || next.at.Sub(previous.at) >= time.Second {
		h.providerMetrics.previous[p.ID] = next
	}
	return result
}
