package temporal

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/client"
)

type steeringClient struct {
	client.Client
	opts   client.UpdateWorkflowOptions
	handle client.WorkflowUpdateHandle
	err    error
}

func (s *steeringClient) UpdateWorkflow(_ context.Context, opts client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	s.opts = opts
	return s.handle, s.err
}

type steeringHandle struct {
	accepted bool
	err      error
}

func (s steeringHandle) WorkflowID() string { return "workflow-1" }
func (s steeringHandle) RunID() string      { return "execution-1" }
func (s steeringHandle) UpdateID() string   { return "follow-up" }
func (s steeringHandle) Get(_ context.Context, valuePtr interface{}) error {
	if s.err != nil {
		return s.err
	}
	*(valuePtr.(*bool)) = s.accepted
	return nil
}

func TestSteerPersistentInputUsesStableUpdateID(t *testing.T) {
	sdk := &steeringClient{handle: steeringHandle{accepted: true}}
	c := &Client{c: sdk}
	input := PersistentInput{InputID: "follow-up", Message: "new direction", Metadata: map[string]string{"source": "web"}}
	accepted, err := c.SteerPersistentInput(t.Context(), "workflow-1", input)
	if err != nil || !accepted {
		t.Fatalf("steering result: accepted=%v err=%v", accepted, err)
	}
	if sdk.opts.WorkflowID != "workflow-1" || sdk.opts.UpdateID != "follow-up" || sdk.opts.UpdateName != PersistentSteerUpdate || sdk.opts.WaitForStage != client.WorkflowUpdateStageCompleted {
		t.Fatalf("unexpected update options: %+v", sdk.opts)
	}
	if len(sdk.opts.Args) != 1 || sdk.opts.Args[0].(PersistentInput).Message != input.Message {
		t.Fatalf("update payload changed: %+v", sdk.opts.Args)
	}
	sdk.handle = steeringHandle{accepted: false}
	if accepted, err := c.SteerPersistentInput(t.Context(), "workflow-1", input); err != nil || accepted {
		t.Fatalf("late update must be rejected: accepted=%v err=%v", accepted, err)
	}
	sdk.handle = steeringHandle{err: errors.New("unavailable")}
	if _, err := c.SteerPersistentInput(t.Context(), "workflow-1", input); err == nil {
		t.Fatal("unknown update outcome was reported as accepted")
	}
}

func TestNormalizeExecutionTrace(t *testing.T) {
	detail := &ExecutionDetail{
		WorkflowID: "workflow-1",
		RunID:      "run-1",
		Status:     "WorkflowExecutionCompleted",
		History: []HistoryEvent{
			activityScheduled(1, "LLMChatActivity", map[string]any{
				"request": map[string]any{"model": "gpt-test", "response_format": map[string]any{"type": "json_schema"}},
			}),
			activityCompleted(2, 1, map[string]any{"response": "model response"}),
			activityScheduled(3, "CallToolActivity", map[string]any{
				"server_name": "files", "tool_name": "read", "arguments": map[string]any{"path": "README.md"},
			}),
			activityCompleted(4, 3, map[string]any{"content": "file contents"}),
			activityScheduled(5, "BuildToolDefsActivity", map[string]any{"agent_id": "agent-1"}),
			{EventID: 6, EventType: "WorkflowExecutionCompleted", Details: map[string]any{
				"workflowExecutionCompletedEventAttributes": map[string]any{"result": payload(map[string]any{"response": "done"})},
			}},
		},
	}

	trace := NormalizeExecutionTrace(detail)
	if trace.WorkflowID != "workflow-1" || trace.RunID != "run-1" || trace.Status != detail.Status {
		t.Fatalf("unexpected trace metadata: %+v", trace)
	}
	if len(trace.Events) != 4 {
		t.Fatalf("events = %d, want 4: %+v", len(trace.Events), trace.Events)
	}
	if event := trace.Events[0]; event.Kind != "model" || event.Model != "gpt-test" || event.Output.(map[string]any)["response"] != "model response" {
		t.Fatalf("unexpected model event: %+v", event)
	}
	if event := trace.Events[1]; event.Kind != "tool" || event.Tool != "files.read" || event.Output.(map[string]any)["content"] != "file contents" {
		t.Fatalf("unexpected tool event: %+v", event)
	}
	if event := trace.Events[2]; event.Kind != "schema" {
		t.Fatalf("unexpected schema event: %+v", event)
	}
	if event := trace.Events[3]; event.Kind != "terminal" || event.Output.(map[string]any)["response"] != "done" {
		t.Fatalf("unexpected terminal event: %+v", event)
	}
}

func activityScheduled(id int64, activity string, input map[string]any) HistoryEvent {
	return HistoryEvent{EventID: id, EventType: "ActivityTaskScheduled", Details: map[string]any{
		"activityTaskScheduledEventAttributes": map[string]any{
			"activityType": map[string]any{"name": activity},
			"input":        payload(input),
		},
	}}
}

func activityCompleted(id, scheduledID int64, result map[string]any) HistoryEvent {
	return HistoryEvent{EventID: id, EventType: "ActivityTaskCompleted", Details: map[string]any{
		"activityTaskCompletedEventAttributes": map[string]any{
			"scheduledEventId": float64(scheduledID),
			"result":           payload(result),
		},
	}}
}

func payload(value map[string]any) map[string]any {
	return map[string]any{"payloads": []any{value}}
}
