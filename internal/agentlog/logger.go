// Package agentlog writes structured, human-readable log files for agent runs.
// One file is created per agent under a configurable logs directory.
// All files are truncated when New() is called, so each process startup
// produces a clean set of logs for that session only.
package agentlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const divider = "================================================================================"

// Logger manages per-agent log files.
type Logger struct {
	dir   string
	mu    sync.Mutex            // protects files map
	files map[string]*agentFile // keyed by agent name
}

// agentFile holds the open file and its write mutex.
type agentFile struct {
	mu sync.Mutex
	f  *os.File
}

// New creates the logs directory, opens (and truncates) one log file for each
// agent name provided, and returns a ready-to-use Logger.
func New(dir string, agentNames []string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("agentlog: create log dir %q: %w", dir, err)
	}

	l := &Logger{
		dir:   dir,
		files: make(map[string]*agentFile, len(agentNames)),
	}

	for _, name := range agentNames {
		af, err := openFile(dir, name)
		if err != nil {
			return nil, err
		}
		l.files[name] = af
	}

	return l, nil
}

// openFile creates or truncates the log file for the given agent name.
func openFile(dir, agentName string) (*agentFile, error) {
	path := filepath.Join(dir, agentName+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("agentlog: open log file %q: %w", path, err)
	}
	return &agentFile{f: f}, nil
}

// fileFor returns the agentFile for name, creating it lazily if it wasn't
// pre-opened (e.g. an agent defined after startup).
func (l *Logger) fileFor(name string) *agentFile {
	l.mu.Lock()
	defer l.mu.Unlock()
	af, ok := l.files[name]
	if !ok {
		af, _ = openFile(l.dir, name) // best-effort; nil handled in write
		l.files[name] = af
	}
	return af
}

// RunLog is a scoped logger for a single agent run.
type RunLog struct {
	af        *agentFile
	agentName string
	model     string
	startedAt time.Time
}

// ForRun returns a RunLog scoped to one agent invocation.
func (l *Logger) ForRun(agentName, model string) *RunLog {
	return &RunLog{
		af:        l.fileFor(agentName),
		agentName: agentName,
		model:     model,
		startedAt: time.Now(),
	}
}

// write is the single low-level write path; it serialises writes per agent file.
func (rl *RunLog) write(s string) {
	if rl == nil || rl.af == nil {
		return
	}
	rl.af.mu.Lock()
	defer rl.af.mu.Unlock()
	fmt.Fprint(rl.af.f, s)
}

// writeln appends a newline after s.
func (rl *RunLog) writeln(s string) { rl.write(s + "\n") }

// ── Public logging methods ────────────────────────────────────────────────────

// Start writes the run header.
func (rl *RunLog) Start(userInput string) {
	ts := rl.startedAt.Format("2006-01-02 15:04:05")
	rl.write("\n" + divider + "\n")
	rl.writeln(fmt.Sprintf("RUN STARTED  %s", ts))
	rl.writeln(fmt.Sprintf("Agent: %s  |  Model: %s", rl.agentName, rl.model))
	rl.writeln(divider)
}

// SystemPrompt logs the agent's system prompt.
func (rl *RunLog) SystemPrompt(prompt string) {
	rl.writeln("\n--- SYSTEM PROMPT ---")
	rl.writeln(indent(prompt, "  "))
}

// UserMessage logs the user's input message.
func (rl *RunLog) UserMessage(msg string) {
	rl.writeln("\n--- USER MESSAGE ---")
	rl.writeln(indent(msg, "  "))
}

// Turn writes a turn header.
func (rl *RunLog) Turn(n int) {
	rl.writeln(fmt.Sprintf("\n--- TURN %d ---", n))
}

// AssistantText logs a final text response from the assistant.
func (rl *RunLog) AssistantText(text string) {
	rl.writeln("\n[ASSISTANT]")
	rl.writeln(indent(text, "  "))
}

// AssistantToolCalls writes the header for a turn where the assistant made tool calls.
func (rl *RunLog) AssistantToolCalls() {
	rl.writeln("\n[ASSISTANT - tool calls]")
}

// ToolCall logs a single tool call (name + pretty-printed arguments).
func (rl *RunLog) ToolCall(llmName string, rawArgs string) {
	rl.writeln(fmt.Sprintf("\n  > %s", llmName))
	prettyArgs := prettyJSON(rawArgs)
	rl.writeln(indent(prettyArgs, "    "))
}

// SubAgentCall logs when a sub-agent is invoked as a tool.
func (rl *RunLog) SubAgentCall(agentName, message string) {
	rl.writeln(fmt.Sprintf("\n  [SUB-AGENT: %s]", agentName))
	rl.writeln(indent(message, "    "))
}

// Completed writes the run footer with timing and turn count.
func (rl *RunLog) Completed(turns int) {
	dur := time.Since(rl.startedAt).Round(time.Millisecond)
	rl.writeln(fmt.Sprintf("\n%s", divider))
	rl.writeln(fmt.Sprintf("RUN COMPLETED  %s  |  turns: %d  |  duration: %s",
		time.Now().Format("2006-01-02 15:04:05"), turns, dur))
	rl.writeln(divider)
}

// Failed writes an error footer.
func (rl *RunLog) Failed(err error) {
	dur := time.Since(rl.startedAt).Round(time.Millisecond)
	rl.writeln(fmt.Sprintf("\n%s", divider))
	rl.writeln(fmt.Sprintf("RUN FAILED  %s  |  duration: %s",
		time.Now().Format("2006-01-02 15:04:05"), dur))
	rl.writeln(fmt.Sprintf("Error: %s", err))
	rl.writeln(divider)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// indent prefixes every line of s with the given prefix string.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// prettyJSON attempts to pretty-print raw JSON; falls back to the original string.
func prettyJSON(raw string) string {
	if raw == "" || raw == "{}" || raw == "null" {
		return "(no arguments)"
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(b)
}
