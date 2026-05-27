package temporal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	LLMConfig      *LLMConfigInput          `json:"llm_config,omitempty"`
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

type ExecutionDetail struct {
	WorkflowID string         `json:"workflow_id"`
	RunID      string         `json:"run_id"`
	AgentName  string         `json:"agent_name"`
	RunType    string         `json:"run_type"`
	Status     string         `json:"status"`
	StartTime  string         `json:"start_time"`
	CloseTime  string         `json:"close_time,omitempty"`
	History    []HistoryEvent `json:"history"`
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
				info.AgentName = string(v.GetData())
			}
			if v, ok := attrs["RunType"]; ok {
				info.RunType = string(v.GetData())
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
	firstEvent := true
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("read history event: %w", err)
		}

		eventType := event.GetEventType().String()
		eventTime := ""
		if ts := event.GetEventTime(); ts != nil {
			eventTime = ts.AsTime().Format(time.RFC3339)
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
						detail.AgentName = string(v.GetData())
					}
					if v, ok := sa["RunType"]; ok {
						detail.RunType = string(v.GetData())
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
	}

	detail.History = history
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

func (c *Client) Close() {
	c.c.Close()
}

func randomID() string {
	var buf [8]byte
	rand.Read(buf[:])
	return fmt.Sprintf("%x", buf[:])
}
