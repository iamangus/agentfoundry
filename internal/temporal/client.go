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
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	TaskQueue    = "agentfoundry-worker"
	WorkflowType = "RunAgentWorkflow"
)

type LLMConfigInput struct {
	SchemaValidation bool `json:"schema_validation"`
}

type RunAgentParams struct {
	AgentID        string                   `json:"agent_id"`
	AgentName      string                   `json:"agent_name"`
	Message        string                   `json:"message"`
	History        []llm.Message            `json:"history,omitempty"`
	MCPServers     []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
	ResponseSchema *config.StructuredOutput `json:"response_schema,omitempty"`
	StreamID       string                   `json:"stream_id,omitempty"`
	LLMConfig           *LLMConfigInput          `json:"llm_config,omitempty"`
	MemoryEnabled       bool                     `json:"memory_enabled,omitempty"`
	MemorySearchAgentID string                   `json:"memory_search_agent_id,omitempty"`
	MemoryIngestAgentID string                   `json:"memory_ingest_agent_id,omitempty"`
}

type RunAgentResult struct {
	Response string        `json:"response"`
	History  []llm.Message `json:"history,omitempty"`
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
	if params.StreamID != "" {
		return "chat"
	}
	return "direct"
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

func (c *Client) CancelWorkflow(ctx context.Context, workflowID string) error {
	err := c.c.CancelWorkflow(ctx, workflowID, "")
	if err != nil {
		return fmt.Errorf("cancel workflow %s: %w", workflowID, err)
	}
	slog.Info("canceled temporal workflow", "workflow_id", workflowID)
	return nil
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

type spanDatum struct {
	id              int64
	eventType       string
	ts              time.Time
	name            string
	scheduledEventID int64
	initiatedEventID int64
	timerID         string
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
