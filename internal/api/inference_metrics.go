package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/store"
)

type inferenceObserver struct {
	record                              store.InferenceAttempt
	started                             time.Time
	firstToken, lastToken, firstVisible time.Time
	finished                            bool
	streamDone                          bool
}

func newInferenceObserver(r *http.Request, agentID, providerID, model string, backup bool) *inferenceObserver {
	started := time.Now()
	read := func(name string) string {
		value := r.Header.Get("X-AgentFoundry-" + name)
		if len(value) > 256 {
			return ""
		}
		return value
	}
	number := func(name string) int {
		n, _ := strconv.Atoi(read(name))
		if n < 1 {
			return 1
		}
		return n
	}
	o := &inferenceObserver{started: started, record: store.InferenceAttempt{AgentID: agentID, ProviderID: providerID, Model: model, Backup: backup, RunID: read("Run-ID"), WorkflowID: read("Workflow-ID"), ExecutionID: read("Execution-ID"), ActivityID: read("Activity-ID"), ActivityAttempt: number("Activity-Attempt"), HTTPAttempt: number("HTTP-Attempt"), StartedAt: started.UTC(), Status: "failed", Metrics: store.InferenceMetrics{TimingSource: "agentfoundry_proxy"}}}
	if o.record.ExecutionID != "" && o.record.ActivityID != "" {
		key, _ := json.Marshal([]any{o.record.ExecutionID, o.record.ActivityID, o.record.ActivityAttempt, o.record.HTTPAttempt})
		sum := sha256.Sum256(key)
		o.record.ID = hex.EncodeToString(sum[:])
	} else {
		var b [16]byte
		_, _ = rand.Read(b[:])
		o.record.ID = hex.EncodeToString(b[:])
	}
	return o
}

func (o *inferenceObserver) observe(data []byte, streaming bool) {
	if string(data) == "[DONE]" {
		o.finished = true
		o.streamDone = true
		return
	}
	var frame struct {
		Error json.RawMessage `json:"error"`
		Usage *struct {
			Prompt        *int64 `json:"prompt_tokens"`
			Completion    *int64 `json:"completion_tokens"`
			PromptDetails *struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails *struct {
				Reasoning *int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
		Timings *struct {
			PromptMS    *float64 `json:"prompt_ms"`
			PromptN     *int64   `json:"prompt_n"`
			PredictedMS *float64 `json:"predicted_ms"`
		} `json:"timings"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Content          string            `json:"content"`
				Reasoning        string            `json:"reasoning"`
				ReasoningContent string            `json:"reasoning_content"`
				ReasoningDetails []json.RawMessage `json:"reasoning_details"`
				ToolCalls        []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &frame) != nil {
		return
	}
	if len(frame.Error) > 0 && string(frame.Error) != "null" {
		o.record.Status = "provider_error"
	}
	if frame.Usage != nil {
		u := frame.Usage
		m := &o.record.Metrics
		if u.Prompt != nil && *u.Prompt >= 0 {
			m.InputTokens = u.Prompt
		}
		if u.Completion != nil && *u.Completion >= 0 {
			m.OutputTokens = u.Completion
		}
		if u.PromptDetails != nil && u.PromptDetails.Cached != nil && *u.PromptDetails.Cached >= 0 {
			m.CachedInputTokens = u.PromptDetails.Cached
		}
		if u.CompletionDetails != nil && u.CompletionDetails.Reasoning != nil && *u.CompletionDetails.Reasoning >= 0 {
			m.ReasoningTokens = u.CompletionDetails.Reasoning
		}
	}
	if frame.Timings != nil {
		t := frame.Timings
		m := &o.record.Metrics
		if validMetric(t.PromptMS) {
			m.PrefillMS = t.PromptMS
			m.ProviderTimingSource = "llama.cpp_timings"
		}
		if validMetric(t.PredictedMS) {
			m.ProviderDecodeMS = t.PredictedMS
			m.ProviderTimingSource = "llama.cpp_timings"
		}
		if validMetric(t.PromptMS) && *t.PromptMS > 0 && t.PromptN != nil && *t.PromptN >= 0 {
			v := float64(*t.PromptN) * 1000 / *t.PromptMS
			m.PrefillTokensPerSecond = &v
		}
	}
	if !streaming {
		o.finished = true
		return
	}
	for _, c := range frame.Choices {
		if c.FinishReason != nil {
			o.finished = true
		}
		d := c.Delta
		generated := d.Content != "" || d.Reasoning != "" || d.ReasoningContent != "" || len(d.ReasoningDetails) > 0
		for _, t := range d.ToolCalls {
			generated = generated || t.Function.Name != "" || t.Function.Arguments != ""
		}
		if generated {
			now := time.Now()
			if o.firstToken.IsZero() {
				o.firstToken = now
			}
			o.lastToken = now
			if d.Content != "" && o.firstVisible.IsZero() {
				o.firstVisible = now
			}
		}
	}
}

func validMetric(n *float64) bool {
	return n != nil && *n >= 0 && !math.IsNaN(*n) && !math.IsInf(*n, 0)
}

func (h *Handler) finishInference(r *http.Request, o *inferenceObserver) {
	if h.inferenceStore == nil {
		return
	}
	o.record.FinishedAt = time.Now().UTC()
	m := &o.record.Metrics
	m.DurationMS = float64(time.Since(o.started)) / float64(time.Millisecond)
	if o.finished && o.record.HTTPStatus == 200 && o.record.Status != "provider_error" {
		o.record.Status = "completed"
	} else if r.Context().Err() != nil && o.record.Status != "provider_error" {
		o.record.Status = "canceled"
	}
	if !o.firstToken.IsZero() {
		v := float64(o.firstToken.Sub(o.started)) / float64(time.Millisecond)
		m.TimeToFirstTokenMS = &v
	}
	if !o.firstVisible.IsZero() {
		v := float64(o.firstVisible.Sub(o.started)) / float64(time.Millisecond)
		m.TimeToFirstVisibleTokenMS = &v
	}
	if m.InputTokens != nil && *m.InputTokens > 0 && m.CachedInputTokens != nil && *m.CachedInputTokens <= *m.InputTokens {
		v := float64(*m.CachedInputTokens) / float64(*m.InputTokens)
		m.CacheHitRatio = &v
	}
	if m.OutputTokens != nil && *m.OutputTokens > 1 && !o.firstToken.IsZero() && o.lastToken.After(o.firstToken) {
		v := float64(*m.OutputTokens-1) / o.lastToken.Sub(o.firstToken).Seconds()
		m.ObservedOutputTokensPerSecond = &v
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	if err := h.inferenceStore.Save(ctx, o.record); err != nil {
		slog.Warn("could not save inference metrics", "request_id", o.record.ID, "error", err)
		return
	}
	if o.record.RunID != "" && h.streams != nil {
		payload, _ := json.Marshal(o.record)
		h.streams.PublishEvent(o.record.RunID, "inference_metrics", string(payload))
	}
}

type metricCapture struct {
	bytes.Buffer
	overflow bool
}

func (b *metricCapture) Write(p []byte) (int, error) {
	if b.Len()+len(p) <= 1024*1024 {
		_, _ = b.Buffer.Write(p)
	} else {
		b.overflow = true
	}
	return len(p), nil
}

func (h *Handler) getRunInference(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ru, ok := h.runs.Get(r.PathValue("id"))
	if !ok || (ru.Owner != ac.Subject && !ac.IsGlobalAdmin) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	if h.inferenceStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "inference metrics unavailable"})
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 1024 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
		return
	}
	page, err := h.inferenceStore.List(r.Context(), ru.ID, r.URL.Query().Get("input_id"), cursor)
	if err != nil {
		if strings.Contains(err.Error(), "cursor") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read inference metrics"})
		}
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func copyInferenceResponse(w io.Writer, body io.Reader, o *inferenceObserver) error {
	var captured metricCapture
	_, err := io.Copy(w, io.TeeReader(body, &captured))
	if err == nil && captured.overflow {
		o.finished = true
	}
	if err == nil && !captured.overflow {
		o.observe(captured.Bytes(), false)
	}
	return err
}
