package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type inferenceStreamDelta struct {
	Role             string            `json:"role,omitempty"`
	Content          *string           `json:"content,omitempty"`
	Reasoning        *string           `json:"reasoning,omitempty"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details,omitempty"`
	ToolCalls        []struct {
		Index    int `json:"index"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
	Refusal *string `json:"refusal,omitempty"`
}

type inferenceStreamChoice struct {
	Index        int                  `json:"index"`
	Delta        inferenceStreamDelta `json:"delta"`
	FinishReason *string              `json:"finish_reason"`
}

type inferenceStreamChunk struct {
	ID      string                  `json:"id"`
	Choices []inferenceStreamChoice `json:"choices"`
	Error   *json.RawMessage        `json:"error,omitempty"`
}

func (h *Handler) inferenceProxy(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")

	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent ID is required"})
		return
	}

	if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != h.internalAPIKey {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	def := h.store.GetDefinitionByID(agentID)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}

	providerID := def.ProviderID
	model := def.Model
	backup := r.Header.Get("X-AgentFoundry-Backup") == "true"
	if backup {
		providerID, model = def.BackupProviderID, def.BackupModel
	}
	if providerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent has no provider configured"})
		return
	}

	prov, err := h.providerStore.GetByID(r.Context(), providerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "provider not found: " + err.Error()})
		return
	}

	if prov.BaseURL == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "provider has no base URL"})
		return
	}

	baseURL := strings.TrimRight(prov.BaseURL, "/")
	targetURL := baseURL + "/chat/completions"

	bodyBytes, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		slog.Error("failed to read request body", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read request"})
		return
	}

	if model == "" {
		model = prov.DefaultModel
	}
	// Routing is server-owned: the worker can select only this agent's backup,
	// never an arbitrary provider or credentials. Do not leak primary tuning to
	// a different model; the backup uses its provider's defaults.
	params := def.ModelParams
	if backup {
		params = nil
	}
	bodyBytes, err = prepareInferencePayload(bodyBytes, model, prov.SchemaValidation, prov.Reasoning, params)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		slog.Error("failed to create proxy request", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy error"})
		return
	}

	req.Header.Set("Content-Type", "application/json")
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}
	for k, v := range prov.Headers {
		if k != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("failed to proxy inference request", "provider", prov.Name, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "provider unreachable"})
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-AgentFoundry-Provider", providerID)
	w.Header().Set("X-AgentFoundry-Model", model)
	w.WriteHeader(resp.StatusCode)

	logPath := fmt.Sprintf("/data/inference_%s_%d.log", agentID, time.Now().UnixNano())
	_ = os.MkdirAll("/data", 0755)
	logFile, logErr := os.Create(logPath)
	if logErr != nil {
		slog.Warn("failed to create inference log", "path", logPath, "error", logErr)
	} else {
		defer logFile.Close()
		fmt.Fprintf(logFile, "# agent_id=%s provider=%s model=%s backup=%t\n", agentID, prov.Name, model, backup)
	}

	if flusher, ok := w.(http.Flusher); ok && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()

			if logFile != nil {
				logFile.WriteString(line + "\n")
			}

			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				if data != "[DONE]" {
					var chunk inferenceStreamChunk
					if err := json.Unmarshal([]byte(data), &chunk); err == nil {
						if chunk.Error != nil {
							slog.Error("inference mid-stream error", "agent_id", agentID, "error", string(*chunk.Error))
							if logFile != nil {
								fmt.Fprintf(logFile, "# error: %s\n", string(*chunk.Error))
							}
						}
						for _, choice := range chunk.Choices {
							if choice.Delta.Content != nil && *choice.Delta.Content != "" {
								slog.Debug("inference delta", "agent_id", agentID, "token", *choice.Delta.Content)
								if logFile != nil {
									fmt.Fprintf(logFile, "# token: %q\n", *choice.Delta.Content)
								}
							}
							if choice.Delta.Reasoning != nil && *choice.Delta.Reasoning != "" {
								slog.Debug("inference delta", "agent_id", agentID, "reasoning", *choice.Delta.Reasoning)
								if logFile != nil {
									fmt.Fprintf(logFile, "# reasoning: %q\n", *choice.Delta.Reasoning)
								}
							}
							if len(choice.Delta.ReasoningDetails) > 0 {
								if logFile != nil {
									fmt.Fprintf(logFile, "# reasoning_details: %d items\n", len(choice.Delta.ReasoningDetails))
								}
							}
							for _, tc := range choice.Delta.ToolCalls {
								slog.Info("inference delta", "agent_id", agentID, "tool_call", tc.Function.Name)
								if logFile != nil {
									fmt.Fprintf(logFile, "# tool_call: name=%s args=%q\n", tc.Function.Name, tc.Function.Arguments)
								}
							}
							if choice.Delta.Refusal != nil && *choice.Delta.Refusal != "" {
								slog.Warn("inference delta", "agent_id", agentID, "refusal", *choice.Delta.Refusal)
								if logFile != nil {
									fmt.Fprintf(logFile, "# refusal: %q\n", *choice.Delta.Refusal)
								}
							}
							if choice.FinishReason != nil {
								slog.Info("inference delta", "agent_id", agentID, "finish", *choice.FinishReason)
								if logFile != nil {
									fmt.Fprintf(logFile, "# finish: %s\n", *choice.FinishReason)
								}
							}
						}
					}
				}
			}

			w.Write([]byte(line + "\n"))
			flusher.Flush()
		}
		if err := scanner.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				// The client closed the stream (e.g. the worker moved on to
				// handle tool calls). This is expected, not an error.
				slog.Debug("inference proxy read ended (client canceled)", "agent_id", agentID)
			} else {
				slog.Error("inference proxy read error", "error", err)
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
}

func isNonEmptyJSON(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("{}")) && !bytes.Equal(raw, []byte("null"))
}

func prepareInferencePayload(body []byte, model string, supportsSchema bool, extras ...json.RawMessage) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, fmt.Errorf("invalid inference JSON object")
	}
	if err := json.Unmarshal(mergeInferenceParams(body, extras...), &payload); err != nil {
		return nil, err
	}
	payload["model"] = model
	if format, ok := payload["response_format"].(map[string]any); ok && format["type"] == "json_schema" && !supportsSchema {
		schema, _ := json.Marshal(format["json_schema"])
		if messages, ok := payload["messages"].([]any); ok {
			payload["messages"] = append([]any{map[string]any{"role": "system", "content": "Respond with ONLY valid JSON matching this schema: " + string(schema)}}, messages...)
		}
		payload["response_format"] = map[string]any{"type": "json_object"}
	}
	return json.Marshal(payload)
}

func mergeInferenceParams(body []byte, extras ...json.RawMessage) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	for _, extra := range extras {
		if !isNonEmptyJSON(extra) {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal(extra, &e); err != nil {
			continue
		}
		for k, v := range e {
			m[k] = v
		}
	}
	merged, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return merged
}
