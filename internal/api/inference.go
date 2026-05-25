package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type inferenceStreamDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          *string          `json:"content,omitempty"`
	Reasoning        *string          `json:"reasoning,omitempty"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details,omitempty"`
	ToolCalls        []struct {
		Index    int    `json:"index"`
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

	if def.ProviderID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent has no provider configured"})
		return
	}

	prov, err := h.providerStore.GetByID(r.Context(), def.ProviderID)
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

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, r.Body)
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
	w.WriteHeader(resp.StatusCode)

	logPath := fmt.Sprintf("/data/inference_%s_%d.log", agentID, time.Now().UnixNano())
	_ = os.MkdirAll("/data", 0755)
	logFile, logErr := os.Create(logPath)
	if logErr != nil {
		slog.Warn("failed to create inference log", "path", logPath, "error", logErr)
	} else {
		defer logFile.Close()
		fmt.Fprintf(logFile, "# agent_id=%s provider=%s model=%s\n", agentID, prov.Name, def.Model)
	}

	if flusher, ok := w.(http.Flusher); ok {
		scanner := bufio.NewScanner(resp.Body)
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
			slog.Error("inference proxy read error", "error", err)
		}
	} else {
		io.Copy(w, resp.Body)
	}
}
