package temporal

import "testing"

func TestBackupTraceUsesActualRoutingAndAttempts(t *testing.T) {
	detail := &ExecutionDetail{History: []HistoryEvent{
		activityScheduled(1, "LLMChatActivity", map[string]any{"backup": true, "provider_id": "configured", "failover_reason": "Primary failed after 2 attempts", "request": map[string]any{"model": ""}}),
		activityCompleted(2, 1, map[string]any{"attempts": 2, "response": map[string]any{"model": "provider-default", "provider_id": "actual"}}),
	}}
	trace := NormalizeExecutionTrace(detail)
	if len(trace.Events) != 1 {
		t.Fatalf("trace: %+v", trace)
	}
	event := trace.Events[0]
	if !event.Backup || event.ProviderID != "actual" || event.Model != "provider-default" || event.Attempts != 2 || event.FailoverReason == "" {
		t.Fatalf("lost failover metadata: %+v", event)
	}
}
