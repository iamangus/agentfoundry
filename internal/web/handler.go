package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/angoo/agentfile/internal/agent"
	"github.com/angoo/agentfile/internal/config"
)

//go:embed templates/*.html
var templateFS embed.FS

// DefinitionStore is the subset of config.Loader used by the web handler.
type DefinitionStore interface {
	ListDefinitions() []*config.Definition
	GetRawDefinition(name string) ([]byte, error)
}

// Message is a single turn in a chat.
type Message struct {
	Role    string // "user" or "assistant"
	Content string
	Time    time.Time
}

// Session is an in-memory chat session.
type Session struct {
	ID        string
	AgentName string
	Messages  []Message
	CreatedAt time.Time
}

// runEvent is a single SSE event for an in-flight agent run.
type runEvent struct {
	typ  string // "status" | "done" | "error"
	data string
}

// agentRun tracks an in-flight agent run.
type agentRun struct {
	ch     chan runEvent // closed when the run finishes
	result string        // populated on done
	err    error         // populated on error
}

// chanReporter implements agent.Reporter by sending status updates to a channel.
type chanReporter struct {
	ch chan runEvent
}

func (r *chanReporter) Update(status string) {
	// Non-blocking send — drop the event if no SSE client is connected yet.
	select {
	case r.ch <- runEvent{typ: "status", data: status}:
	default:
	}
}

// Handler serves the web UI pages.
type Handler struct {
	store    DefinitionStore
	runtime  *agent.Runtime
	tmpl     *template.Template
	mu       sync.Mutex
	sessions map[string]*Session
	runs     map[string]*agentRun
}

// NewHandler creates a new web UI handler.
func NewHandler(store DefinitionStore, runtime *agent.Runtime) (*Handler, error) {
	tmpl, err := template.New("").ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Handler{
		store:    store,
		runtime:  runtime,
		tmpl:     tmpl,
		sessions: make(map[string]*Session),
		runs:     make(map[string]*agentRun),
	}, nil
}

// RegisterRoutes registers the web UI routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.redirectToChat)
	mux.HandleFunc("GET /chat", h.chatPage)
	mux.HandleFunc("POST /chat/sessions", h.newSession)
	mux.HandleFunc("GET /chat/sessions/list", h.sessionListPartial)
	mux.HandleFunc("POST /chat/sessions/{id}/messages", h.postMessage)
	mux.HandleFunc("GET /chat/runs/{id}/events", h.runEvents)
	mux.HandleFunc("GET /agents", h.agentsPage)
	mux.HandleFunc("GET /agents/list", h.agentListPartial)
	mux.HandleFunc("GET /agents/{name}/edit", h.agentEditPartial)
	slog.Info("web UI routes registered")
}

func (h *Handler) redirectToChat(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/chat", http.StatusFound)
}

// --- Chat page ---

type chatPageData struct {
	ActivePage string
	Agents     []*config.Definition
	Sessions   []*Session
	Current    *Session
}

func (h *Handler) chatPage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	h.mu.Lock()
	sessions := h.orderedSessions()
	var current *Session
	if sessionID != "" {
		current = h.sessions[sessionID]
	}
	h.mu.Unlock()

	data := chatPageData{
		ActivePage: "chat",
		Agents:     h.store.ListDefinitions(),
		Sessions:   sessions,
		Current:    current,
	}
	h.render(w, "chat.html", data)
}

func (h *Handler) newSession(w http.ResponseWriter, r *http.Request) {
	agentName := r.FormValue("agent")
	if agentName == "" {
		http.Error(w, "agent is required", http.StatusBadRequest)
		return
	}

	id := newID()
	session := &Session{
		ID:        id,
		AgentName: agentName,
		Messages:  []Message{},
		CreatedAt: time.Now(),
	}

	h.mu.Lock()
	h.sessions[id] = session
	sessions := h.orderedSessions()
	h.mu.Unlock()

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Push-Url", "/chat?session="+id)
		data := chatPageData{
			ActivePage: "chat",
			Agents:     h.store.ListDefinitions(),
			Sessions:   sessions,
			Current:    session,
		}
		h.renderPartial(w, "chat-content", data)
		return
	}

	http.Redirect(w, r, "/chat?session="+id, http.StatusSeeOther)
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	content := r.FormValue("message")
	if content == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	session := h.sessions[sessionID]
	h.mu.Unlock()

	if session == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	userMsg := Message{Role: "user", Content: content, Time: time.Now()}
	h.mu.Lock()
	session.Messages = append(session.Messages, userMsg)
	h.mu.Unlock()

	// Look up agent definition
	defs := h.store.ListDefinitions()
	var def *config.Definition
	for _, d := range defs {
		if d.Name == session.AgentName {
			def = d
			break
		}
	}

	// Create a run and start the agent asynchronously.
	runID := newID()
	run := &agentRun{
		// Buffered so the agent goroutine never blocks on status events.
		ch: make(chan runEvent, 32),
	}

	h.mu.Lock()
	h.runs[runID] = run
	h.mu.Unlock()

	go func() {
		// Use a detached context so the agent run isn't cancelled when the
		// POST response is sent and the client connection closes.
		ctx := context.WithoutCancel(r.Context())
		rep := &chanReporter{ch: run.ch}
		var result string
		var err error

		if def == nil {
			err = fmt.Errorf("agent %q not found", session.AgentName)
		} else {
			result, err = h.runtime.RunWithReporter(ctx, def, content, rep)
		}

		// Store result and append to session
		h.mu.Lock()
		run.result = result
		run.err = err
		if err != nil {
			slog.Error("agent run failed", "agent", session.AgentName, "error", err)
			session.Messages = append(session.Messages, Message{
				Role:    "assistant",
				Content: "Error: " + err.Error(),
				Time:    time.Now(),
			})
			run.ch <- runEvent{typ: "error", data: err.Error()}
		} else {
			session.Messages = append(session.Messages, Message{
				Role:    "assistant",
				Content: result,
				Time:    time.Now(),
			})
			run.ch <- runEvent{typ: "done", data: result}
		}
		h.mu.Unlock()
		close(run.ch)
	}()

	// Return the run ID so the client can open an SSE stream.
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, runID)
}

// runEvents streams SSE events for a given run ID.
func (h *Handler) runEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	h.mu.Lock()
	run := h.runs[runID]
	h.mu.Unlock()

	if run == nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	for {
		select {
		case evt, open := <-run.ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.typ, evt.data)
			flusher.Flush()
			if evt.typ == "done" || evt.typ == "error" {
				// Clean up run after sending terminal event
				h.mu.Lock()
				delete(h.runs, runID)
				h.mu.Unlock()
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// --- Agents page ---

type agentsPageData struct {
	ActivePage string
	Agents     []*config.Definition
}

type agentEditorData struct {
	Name    string
	RawYAML string
}

func (h *Handler) agentsPage(w http.ResponseWriter, r *http.Request) {
	data := agentsPageData{
		ActivePage: "agents",
		Agents:     h.store.ListDefinitions(),
	}
	h.render(w, "agents.html", data)
}

func (h *Handler) sessionListPartial(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	sessions := h.orderedSessions()
	h.mu.Unlock()
	data := chatPageData{
		Sessions: sessions,
	}
	h.renderPartial(w, "session-list", data)
}

func (h *Handler) agentListPartial(w http.ResponseWriter, r *http.Request) {
	data := agentsPageData{
		Agents: h.store.ListDefinitions(),
	}
	h.renderPartial(w, "agent-list-items", data)
}

func (h *Handler) agentEditPartial(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	raw, err := h.store.GetRawDefinition(name)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	data := agentEditorData{
		Name:    name,
		RawYAML: string(raw),
	}
	h.renderPartial(w, "agent-editor", data)
}

// --- helpers ---

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render template", "name", name, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (h *Handler) renderPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render partial", "name", name, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (h *Handler) orderedSessions() []*Session {
	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	for i := 0; i < len(out)-1; i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func newID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
