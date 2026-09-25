package temporal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/llm"
	"github.com/angoo/agentfoundry/internal/mcpclient"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/history/v1"
	"go.temporal.io/api/operatorservice/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	TaskQueue              = "agentfoundry-worker"
	WorkflowType           = "RunAgentWorkflow"
	PersistentWorkflowType = "PersistentRunWorkflow"
	PersistentInputSignal  = "persistent-input"
)

type LLMConfigInput struct {
	SchemaValidation bool `json:"schema_validation"`
}

type RunAgentParams struct {
	AgentID             string                   `json:"agent_id"`
	AgentName           string                   `json:"agent_name"`
	Message             string                   `json:"message"`
	History             []llm.Message            `json:"history,omitempty"`
	MCPServers          []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
	ResponseSchema      *config.StructuredOutput `json:"response_schema,omitempty"`
	StreamID            string                   `json:"stream_id,omitempty"`
	SessionID           string                   `json:"session_id,omitempty"`
	LLMConfig           *LLMConfigInput          `json:"llm_config,omitempty"`
	MemoryEnabled       bool                     `json:"memory_enabled,omitempty"`
	MemorySearchAgentID string                   `json:"memory_search_agent_id,omitempty"`
	MemoryIngestAgentID string                   `json:"memory_ingest_agent_id,omitempty"`
	UserSubject         string                   `json:"user_subject,omitempty"`
	HandoffCount        int                      `json:"handoff_count,omitempty"`
}

type RunAgentResult struct {
	Response string        `json:"response"`
	History  []llm.Message `json:"history,omitempty"`
}

type PersistentInput struct {
	Message  string            `json:"message"`
	InputID  string            `json:"input_id"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Client struct {
	c         client.Client
	namespace string
}

type Config struct {
	HostPort  string
	Namespace string
	APIKey    string
}

func NewClient(hostPort, namespace, apiKey string) (*Client, error) {
	opts := client.Options{
		HostPort:  hostPort,
		Namespace: namespace,
	}
	if apiKey != "" {
		opts.Credentials = client.NewAPIKeyStaticCredentials(apiKey)
	}

	c, err := client.Dial(opts)
	if err != nil {
		return nil, fmt.Errorf("dial temporal: %w", err)
	}
	slog.Info("connected to temporal server", "host", hostPort, "namespace", namespace)
	return &Client{c: c, namespace: namespace}, nil
}

func (c *Client) runType(params *RunAgentParams) string {
	if params.SessionID != "" {
		return "session"
	}
	return "stateless"
}

func (c *Client) searchAttrs(params *RunAgentParams) map[string]interface{} {
	return map[string]interface{}{
		"AgentName": params.AgentName,
		"RunType":   c.runType(params),
	}
}

func (c *Client) ExecuteWorkflow(ctx context.Context, params RunAgentParams) (string, error) {
	workflowID := params.AgentID + "-" + randomID()
	workflowOpts := client.StartWorkflowOptions{
		ID:               workflowID,
		TaskQueue:        TaskQueue,
		SearchAttributes: c.searchAttrs(&params),
	}

	run, err := c.c.ExecuteWorkflow(ctx, workflowOpts, WorkflowType, params)
	if err != nil {
		return "", fmt.Errorf("start workflow: %w", err)
	}
	slog.Info("started temporal workflow", "workflow_id", workflowID, "agent", params.AgentName)

	return run.GetID(), nil
}

func (c *Client) ExecuteWorkflowSync(ctx context.Context, params RunAgentParams) (*RunAgentResult, error) {
	workflowID := params.AgentID + "-" + randomID()
	workflowOpts := client.StartWorkflowOptions{
		ID:               workflowID,
		TaskQueue:        TaskQueue,
		SearchAttributes: c.searchAttrs(&params),
	}

	run, err := c.c.ExecuteWorkflow(ctx, workflowOpts, WorkflowType, params)
	if err != nil {
		return nil, fmt.Errorf("start workflow: %w", err)
	}
	slog.Info("started temporal workflow (sync)", "workflow_id", workflowID, "agent", params.AgentName)

	var result RunAgentResult
	if err := run.Get(ctx, &result); err != nil {
		return nil, fmt.Errorf("workflow execution: %w", err)
	}
	return &result, nil
}

func (c *Client) StartWorkflow(ctx context.Context, params RunAgentParams) (workflowID string, await func(context.Context) (*RunAgentResult, error), err error) {
	workflowID = params.AgentID + "-" + randomID()
	if params.StreamID != "" {
		workflowID = WorkflowIDForRun(params.StreamID, false)
	}
	workflowOpts := client.StartWorkflowOptions{
		ID:               workflowID,
		TaskQueue:        TaskQueue,
		SearchAttributes: c.searchAttrs(&params),
	}

	wfRun, err := c.c.ExecuteWorkflow(ctx, workflowOpts, WorkflowType, params)
	if err != nil {
		return "", nil, fmt.Errorf("start workflow: %w", err)
	}
	slog.Info("started temporal workflow (async)", "workflow_id", workflowID, "agent", params.AgentName)

	await = func(ctx context.Context) (*RunAgentResult, error) {
		var result RunAgentResult
		if err := wfRun.Get(ctx, &result); err != nil {
			return nil, fmt.Errorf("workflow execution: %w", err)
		}
		return &result, nil
	}
	return workflowID, await, nil
}

func (c *Client) StartPersistentWorkflow(ctx context.Context, runID string, params RunAgentParams) (workflowID string, await func(context.Context) error, err error) {
	workflowID = WorkflowIDForRun(runID, true)
	wfRun, err := c.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: workflowID, TaskQueue: TaskQueue, SearchAttributes: c.searchAttrs(&params),
	}, PersistentWorkflowType, params)
	if err != nil {
		return "", nil, fmt.Errorf("start persistent workflow: %w", err)
	}
	await = func(ctx context.Context) error {
		return wfRun.Get(ctx, nil)
	}
	return workflowID, await, nil
}

func WorkflowIDForRun(runID string, persistent bool) string {
	if persistent {
		return "persistent-" + runID
	}
	return "run-" + runID
}

func (c *Client) SignalPersistentInput(ctx context.Context, workflowID string, input PersistentInput) error {
	if err := c.c.SignalWorkflow(ctx, workflowID, "", PersistentInputSignal, input); err != nil {
		return fmt.Errorf("signal persistent workflow: %w", err)
	}
	return nil
}

func (c *Client) CancelWorkflow(ctx context.Context, workflowID string) error {
	err := c.c.CancelWorkflow(ctx, workflowID, "")
	if err != nil {
		return fmt.Errorf("cancel workflow %s: %w", workflowID, err)
	}
	slog.Info("canceled temporal workflow", "workflow_id", workflowID)
	return nil
}

func (c *Client) AwaitWorkflow(ctx context.Context, workflowID string, persistent bool) (*RunAgentResult, error) {
	return awaitWorkflow(ctx, persistent, time.Second, func(result *RunAgentResult) error {
		if persistent {
			return c.c.GetWorkflow(ctx, workflowID, "").Get(ctx, nil)
		}
		return c.c.GetWorkflow(ctx, workflowID, "").Get(ctx, result)
	})
}

func awaitWorkflow(ctx context.Context, persistent bool, interval time.Duration, get func(*RunAgentResult) error) (*RunAgentResult, error) {
	for attempt := 0; ; attempt++ {
		var result RunAgentResult
		err := get(&result)
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) && attempt < 60 || transientWorkflowLookup(err) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(interval):
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if persistent {
			return nil, nil
		}
		return &result, nil
	}
}

func transientWorkflowLookup(err error) bool {
	if err == nil {
		return false
	}
	var unavailable *serviceerror.Unavailable
	if errors.As(err, &unavailable) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return true
	}
	return false
}

type ExecutionInfo struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	AgentName  string `json:"agent_name"`
	RunType    string `json:"run_type"`
	Status     string `json:"status"`
	StartTime  string `json:"start_time"`
	CloseTime  string `json:"close_time,omitempty"`
}

type HistoryEvent struct {
	EventID   int64       `json:"event_id"`
	EventType string      `json:"event_type"`
	EventTime string      `json:"event_time"`
	Summary   string      `json:"summary"`
	Details   interface{} `json:"details,omitempty"`
}

type TimelineSpan struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	StartTime    string `json:"start_time"`
	EndTime      string `json:"end_time,omitempty"`
	StartEventID int64  `json:"start_event_id"`
	EndEventID   int64  `json:"end_event_id"`
}

type ExecutionDetail struct {
	WorkflowID string         `json:"workflow_id"`
	RunID      string         `json:"run_id"`
	AgentName  string         `json:"agent_name"`
	RunType    string         `json:"run_type"`
	Status     string         `json:"status"`
	StartTime  string         `json:"start_time"`
	CloseTime  string         `json:"close_time,omitempty"`
	History    []HistoryEvent `json:"history"`
	Spans      []TimelineSpan `json:"spans"`
}

// ExecutionTrace is a compact, provider-neutral view of the activities that
// make up an agent execution. Input and output preserve decoded Temporal data
// for consumers that need more detail than the normalized fields provide.
type ExecutionTrace struct {
	WorkflowID string       `json:"workflow_id"`
	RunID      string       `json:"run_id,omitempty"`
	AgentName  string       `json:"agent_name,omitempty"`
	Status     string       `json:"status"`
	Events     []TraceEvent `json:"events"`
}

type TraceEvent struct {
	EventID   int64  `json:"event_id"`
	EventTime string `json:"event_time"`
	Kind      string `json:"kind"`
	Activity  string `json:"activity,omitempty"`
	Model     string `json:"model,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Schema    any    `json:"schema,omitempty"`
	Input     any    `json:"input,omitempty"`
	Output    any    `json:"output,omitempty"`
	Error     any    `json:"error,omitempty"`
}

type spanDatum struct {
	id               int64
	eventType        string
	ts               time.Time
	name             string
	scheduledEventID int64
	initiatedEventID int64
	timerID          string
}

func (c *Client) EnsureSearchAttributes(ctx context.Context) error {
	_, err := c.c.OperatorService().AddSearchAttributes(ctx, &operatorservice.AddSearchAttributesRequest{
		Namespace: c.namespace,
		SearchAttributes: map[string]enums.IndexedValueType{
			"AgentName": enums.INDEXED_VALUE_TYPE_TEXT,
			"RunType":   enums.INDEXED_VALUE_TYPE_KEYWORD,
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		slog.Warn("failed to register search attributes (may already exist)", "error", err)
	}
	return nil
}

func (c *Client) ListWorkflows(ctx context.Context, query string, pageSize int32) ([]ExecutionInfo, error) {
	if pageSize <= 0 || pageSize > 250 {
		pageSize = 100
	}

	req := &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: c.namespace,
		Query:     query,
		PageSize:  pageSize,
	}

	resp, err := c.c.ListWorkflow(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}

	var execs []ExecutionInfo
	for _, exec := range resp.Executions {
		info := ExecutionInfo{
			WorkflowID: exec.Execution.WorkflowId,
			RunID:      exec.Execution.RunId,
			Status:     exec.Status.String(),
		}
		if ts := exec.StartTime; ts != nil {
			info.StartTime = ts.AsTime().Format(time.RFC3339)
		}
		if ts := exec.CloseTime; ts != nil {
			info.CloseTime = ts.AsTime().Format(time.RFC3339)
		}
		if attrs := exec.GetSearchAttributes().GetIndexedFields(); attrs != nil {
			if v, ok := attrs["AgentName"]; ok {
				info.AgentName = decodePayloadString(v.GetData())
			}
			if v, ok := attrs["RunType"]; ok {
				info.RunType = decodePayloadString(v.GetData())
			}
		}
		execs = append(execs, info)
	}

	return execs, nil
}

func (c *Client) GetWorkflowHistory(ctx context.Context, workflowID, runID string) (*ExecutionDetail, error) {
	iter := c.c.GetWorkflowHistory(ctx, workflowID, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)

	detail := &ExecutionDetail{
		WorkflowID: workflowID,
		RunID:      runID,
	}

	var history []HistoryEvent
	var spanData []spanDatum
	firstEvent := true
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("read history event: %w", err)
		}

		eventType := event.GetEventType().String()
		if strings.HasPrefix(eventType, "WorkflowTask") {
			continue
		}
		var eventTs time.Time
		eventTime := ""
		if ts := event.GetEventTime(); ts != nil {
			eventTs = ts.AsTime()
			eventTime = eventTs.Format(time.RFC3339Nano)
		}

		he := HistoryEvent{
			EventID:   event.GetEventId(),
			EventType: eventType,
			EventTime: eventTime,
			Summary:   summarizeEvent(event),
		}

		if b, err := protojson.Marshal(event); err == nil {
			var raw map[string]interface{}
			if json.Unmarshal(b, &raw) == nil {
				delete(raw, "eventId")
				delete(raw, "eventType")
				delete(raw, "eventTime")
				delete(raw, "version")
				delete(raw, "taskId")
				decodeHistoryPayloads(raw)
				he.Details = raw
			}
		}

		if firstEvent {
			if wfStarted := event.GetWorkflowExecutionStartedEventAttributes(); wfStarted != nil {
				if sa := wfStarted.GetSearchAttributes().GetIndexedFields(); sa != nil {
					if v, ok := sa["AgentName"]; ok {
						detail.AgentName = decodePayloadString(v.GetData())
					}
					if v, ok := sa["RunType"]; ok {
						detail.RunType = decodePayloadString(v.GetData())
					}
				}
			}
			firstEvent = false
		}

		if eventType == "WorkflowExecutionCompleted" || eventType == "WorkflowExecutionFailed" ||
			eventType == "WorkflowExecutionCanceled" || eventType == "WorkflowExecutionTerminated" ||
			eventType == "WorkflowExecutionTimedOut" {
			detail.Status = eventType
			detail.CloseTime = eventTime
		}

		history = append(history, he)
		spanData = append(spanData, collectSpanDatum(event, eventTs))
	}

	detail.History = history
	detail.Spans = buildTimeline(spanData)
	if detail.Status == "" {
		detail.Status = "Running"
	}
	if detail.StartTime == "" && len(history) > 0 {
		detail.StartTime = history[0].EventTime
	}

	return detail, nil
}

func (c *Client) GetExecutionTrace(ctx context.Context, workflowID, runID string) (*ExecutionTrace, error) {
	detail, err := c.GetWorkflowHistory(ctx, workflowID, runID)
	if err != nil {
		return nil, err
	}
	return NormalizeExecutionTrace(detail), nil
}

// NormalizeExecutionTrace folds Temporal activity scheduling and completion
// records into one event per useful agent operation.
func NormalizeExecutionTrace(detail *ExecutionDetail) *ExecutionTrace {
	trace := &ExecutionTrace{
		WorkflowID: detail.WorkflowID,
		RunID:      detail.RunID,
		AgentName:  detail.AgentName,
		Status:     detail.Status,
		Events:     make([]TraceEvent, 0),
	}
	scheduled := make(map[int64]int)

	for _, event := range detail.History {
		details, _ := event.Details.(map[string]interface{})
		switch event.EventType {
		case "ActivityTaskScheduled":
			attrs := mapValue(details, "activityTaskScheduledEventAttributes")
			activity := stringValue(attrs, "activityType", "name")
			kind := traceActivityKind(activity)
			if kind == "" {
				continue
			}
			input := unwrapPayload(mapValue(attrs, "input"))
			traceEvent := TraceEvent{
				EventID: event.EventID, EventTime: event.EventTime, Kind: kind,
				Activity: activity, Input: input,
			}
			switch kind {
			case "model":
				traceEvent.Model = stringValue(mapValue(input, "request"), "model")
				traceEvent.Schema = mapValue(input, "request")["response_format"]
			case "tool":
				server := stringValue(input, "server_name")
				tool := stringValue(input, "tool_name")
				traceEvent.Tool = strings.TrimPrefix(server+"."+tool, ".")
			case "schema":
				traceEvent.Schema = input
			}
			scheduled[event.EventID] = len(trace.Events)
			trace.Events = append(trace.Events, traceEvent)
		case "ActivityTaskCompleted", "ActivityTaskFailed", "ActivityTaskTimedOut", "ActivityTaskCanceled":
			attrs := mapValue(details, strings.ToLower(event.EventType[:1])+event.EventType[1:]+"EventAttributes")
			index, ok := scheduled[intValue(attrs, "scheduledEventId")]
			if !ok {
				continue
			}
			if event.EventType == "ActivityTaskCompleted" {
				trace.Events[index].Output = unwrapPayload(mapValue(attrs, "result"))
			} else {
				trace.Events[index].Error = mapValue(attrs, "failure")
			}
		case "WorkflowExecutionCompleted", "WorkflowExecutionFailed", "WorkflowExecutionCanceled", "WorkflowExecutionTerminated", "WorkflowExecutionTimedOut":
			attrs := mapValue(details, strings.ToLower(event.EventType[:1])+event.EventType[1:]+"EventAttributes")
			terminal := TraceEvent{EventID: event.EventID, EventTime: event.EventTime, Kind: "terminal"}
			if event.EventType == "WorkflowExecutionCompleted" {
				terminal.Output = unwrapPayload(mapValue(attrs, "result"))
			} else {
				terminal.Error = mapValue(attrs, "failure")
			}
			trace.Events = append(trace.Events, terminal)
		}
	}
	return trace
}

func traceActivityKind(activity string) string {
	switch {
	case strings.Contains(activity, "LLMChatActivity"):
		return "model"
	case strings.Contains(activity, "CallToolActivity"):
		return "tool"
	case strings.Contains(activity, "BuildToolDefsActivity"):
		return "schema"
	default:
		return ""
	}
}

func mapValue(value any, keys ...string) map[string]interface{} {
	m, _ := valueAt(value, keys...).(map[string]interface{})
	return m
}

func stringValue(value any, keys ...string) string {
	s, _ := valueAt(value, keys...).(string)
	return s
}

func intValue(value any, key string) int64 {
	if n, ok := valueAt(value, key).(float64); ok {
		return int64(n)
	}
	return 0
}

func valueAt(value any, keys ...string) any {
	for _, key := range keys {
		m, ok := value.(map[string]interface{})
		if !ok {
			return nil
		}
		value = m[key]
	}
	return value
}

func unwrapPayload(value any) any {
	m, ok := value.(map[string]interface{})
	if !ok {
		return value
	}
	payloads, ok := m["payloads"].([]interface{})
	if !ok || len(payloads) != 1 {
		return value
	}
	return payloads[0]
}

func summarizeEvent(event *history.HistoryEvent) string {
	switch {
	case event.GetWorkflowExecutionStartedEventAttributes() != nil:
		return "Workflow execution started"
	case event.GetWorkflowExecutionCompletedEventAttributes() != nil:
		return "Workflow execution completed"
	case event.GetWorkflowExecutionFailedEventAttributes() != nil:
		return "Workflow execution failed"
	case event.GetWorkflowExecutionCanceledEventAttributes() != nil:
		return "Workflow execution canceled"
	case event.GetWorkflowExecutionTerminatedEventAttributes() != nil:
		return "Workflow execution terminated"
	case event.GetWorkflowExecutionTimedOutEventAttributes() != nil:
		return "Workflow execution timed out"
	case event.GetActivityTaskScheduledEventAttributes() != nil:
		attr := event.GetActivityTaskScheduledEventAttributes()
		return "Activity scheduled: " + attr.ActivityType.GetName()
	case event.GetActivityTaskStartedEventAttributes() != nil:
		return "Activity started"
	case event.GetActivityTaskCompletedEventAttributes() != nil:
		return "Activity completed"
	case event.GetActivityTaskFailedEventAttributes() != nil:
		return "Activity failed"
	case event.GetActivityTaskTimedOutEventAttributes() != nil:
		return "Activity timed out"
	case event.GetActivityTaskCanceledEventAttributes() != nil:
		return "Activity canceled"
	case event.GetStartChildWorkflowExecutionInitiatedEventAttributes() != nil:
		attr := event.GetStartChildWorkflowExecutionInitiatedEventAttributes()
		return "Child workflow initiated: " + attr.WorkflowType.GetName()
	case event.GetChildWorkflowExecutionStartedEventAttributes() != nil:
		return "Child workflow started"
	case event.GetChildWorkflowExecutionCompletedEventAttributes() != nil:
		return "Child workflow completed"
	case event.GetChildWorkflowExecutionFailedEventAttributes() != nil:
		return "Child workflow failed"
	case event.GetTimerStartedEventAttributes() != nil:
		return "Timer started"
	case event.GetTimerFiredEventAttributes() != nil:
		return "Timer fired"
	case event.GetWorkflowTaskScheduledEventAttributes() != nil:
		return "Workflow task scheduled"
	case event.GetWorkflowTaskStartedEventAttributes() != nil:
		return "Workflow task started"
	case event.GetWorkflowTaskCompletedEventAttributes() != nil:
		return "Workflow task completed"
	default:
		return event.GetEventType().String()
	}
}

func decodeHistoryPayloads(v interface{}) {
	switch val := v.(type) {
	case map[string]interface{}:
		if data, ok := val["data"].(string); ok {
			if meta, ok := val["metadata"].(map[string]interface{}); ok {
				if encStr, ok := meta["encoding"].(string); ok {
					encBytes, _ := base64.StdEncoding.DecodeString(encStr)
					encoding := string(encBytes)
					dataBytes, _ := base64.StdEncoding.DecodeString(data)
					if encoding == "json/plain" {
						var decoded interface{}
						if json.Unmarshal(dataBytes, &decoded) == nil {
							decodeHistoryPayloads(decoded)
							for k := range val {
								delete(val, k)
							}
							if m, ok := decoded.(map[string]interface{}); ok {
								for k, v := range m {
									val[k] = v
								}
							}
							return
						}
					}
				}
			}
		}
		if payloads, ok := val["payloads"].([]interface{}); ok {
			for _, p := range payloads {
				decodeHistoryPayloads(p)
			}
		}
		for _, child := range val {
			decodeHistoryPayloads(child)
		}
	case []interface{}:
		for _, item := range val {
			decodeHistoryPayloads(item)
		}
	}
}

func collectSpanDatum(event *history.HistoryEvent, ts time.Time) spanDatum {
	sd := spanDatum{id: event.GetEventId(), ts: ts, eventType: event.GetEventType().String()}

	if attr := event.GetActivityTaskScheduledEventAttributes(); attr != nil {
		sd.name = attr.ActivityType.GetName()
	} else if attr := event.GetActivityTaskCompletedEventAttributes(); attr != nil {
		sd.scheduledEventID = attr.GetScheduledEventId()
	} else if attr := event.GetActivityTaskFailedEventAttributes(); attr != nil {
		sd.scheduledEventID = attr.GetScheduledEventId()
	} else if attr := event.GetActivityTaskTimedOutEventAttributes(); attr != nil {
		sd.scheduledEventID = attr.GetScheduledEventId()
	} else if attr := event.GetActivityTaskCanceledEventAttributes(); attr != nil {
		sd.scheduledEventID = attr.GetScheduledEventId()
	} else if attr := event.GetStartChildWorkflowExecutionInitiatedEventAttributes(); attr != nil {
		sd.name = attr.WorkflowType.GetName()
	} else if attr := event.GetChildWorkflowExecutionCompletedEventAttributes(); attr != nil {
		sd.initiatedEventID = attr.GetInitiatedEventId()
	} else if attr := event.GetChildWorkflowExecutionFailedEventAttributes(); attr != nil {
		sd.initiatedEventID = attr.GetInitiatedEventId()
	} else if attr := event.GetChildWorkflowExecutionTimedOutEventAttributes(); attr != nil {
		sd.initiatedEventID = attr.GetInitiatedEventId()
	} else if attr := event.GetChildWorkflowExecutionTerminatedEventAttributes(); attr != nil {
		sd.initiatedEventID = attr.GetInitiatedEventId()
	} else if attr := event.GetChildWorkflowExecutionCanceledEventAttributes(); attr != nil {
		sd.initiatedEventID = attr.GetInitiatedEventId()
	} else if attr := event.GetTimerStartedEventAttributes(); attr != nil {
		sd.timerID = attr.GetTimerId()
	} else if attr := event.GetTimerFiredEventAttributes(); attr != nil {
		sd.timerID = attr.GetTimerId()
	}

	return sd
}

func buildTimeline(data []spanDatum) []TimelineSpan {
	activityStarts := map[int64]*spanDatum{}
	childWfStarts := map[int64]*spanDatum{}
	timerStarts := map[string]*spanDatum{}

	for _, d := range data {
		switch d.eventType {
		case "ActivityTaskScheduled":
			copy := d
			activityStarts[d.id] = &copy
		case "StartChildWorkflowExecutionInitiated":
			copy := d
			childWfStarts[d.id] = &copy
		case "TimerStarted":
			copy := d
			timerStarts[d.timerID] = &copy
		}
	}

	var spans []TimelineSpan

	for _, d := range data {
		switch d.eventType {
		case "ActivityTaskCompleted", "ActivityTaskFailed", "ActivityTaskTimedOut", "ActivityTaskCanceled":
			if start, ok := activityStarts[d.scheduledEventID]; ok {
				spans = append(spans, makeSpan(start, &d, "activity"))
				delete(activityStarts, d.scheduledEventID)
			}
		case "ChildWorkflowExecutionCompleted", "ChildWorkflowExecutionFailed",
			"ChildWorkflowExecutionTimedOut", "ChildWorkflowExecutionTerminated",
			"ChildWorkflowExecutionCanceled":
			if start, ok := childWfStarts[d.initiatedEventID]; ok {
				spans = append(spans, makeSpan(start, &d, "child_workflow"))
				delete(childWfStarts, d.initiatedEventID)
			}
		case "TimerFired":
			if start, ok := timerStarts[d.timerID]; ok {
				spans = append(spans, makeSpan(start, &d, "timer"))
				delete(timerStarts, d.timerID)
			}
		}
	}

	for _, start := range activityStarts {
		spans = append(spans, openSpan(start, "activity"))
	}
	for _, start := range childWfStarts {
		spans = append(spans, openSpan(start, "child_workflow"))
	}
	for _, start := range timerStarts {
		spans = append(spans, openSpan(start, "timer"))
	}

	return spans
}

func makeSpan(start, end *spanDatum, spanType string) TimelineSpan {
	return TimelineSpan{
		ID:           fmt.Sprintf("%s-%d-%d", spanType, start.id, end.id),
		Name:         start.name,
		Type:         spanType,
		StartTime:    start.ts.Format(time.RFC3339Nano),
		EndTime:      end.ts.Format(time.RFC3339Nano),
		StartEventID: start.id,
		EndEventID:   end.id,
	}
}

func openSpan(start *spanDatum, spanType string) TimelineSpan {
	return TimelineSpan{
		ID:           fmt.Sprintf("%s-%d-open", spanType, start.id),
		Name:         start.name,
		Type:         spanType,
		StartTime:    start.ts.Format(time.RFC3339Nano),
		StartEventID: start.id,
	}
}

func (c *Client) Close() {
	c.c.Close()
}

func decodePayloadString(data []byte) string {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return string(data)
	}
	return s
}

func randomID() string {
	var buf [8]byte
	rand.Read(buf[:])
	return fmt.Sprintf("%x", buf[:])
}
