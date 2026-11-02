package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInferencePayloadRoutingAndSchemaCompatibility(t *testing.T) {
	body := []byte(`{"model":"primary","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function"}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}}`)
	for _, native := range []bool{true, false} {
		result, err := prepareInferencePayload(body, "backup", native, json.RawMessage(`{"reasoning":{"effort":"low"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(result, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "backup" || payload["tools"] == nil || payload["reasoning"] == nil {
			t.Fatalf("lost routing/tool/defaults: %s", result)
		}
		format := payload["response_format"].(map[string]any)["type"]
		if native && format != "json_schema" || !native && format != "json_object" {
			t.Fatalf("wrong format: %s", result)
		}
		messages := payload["messages"].([]any)
		if !native && (len(messages) != 2 || !strings.Contains(messages[0].(map[string]any)["content"].(string), `"type":"object"`)) {
			t.Fatalf("lost schema instruction: %s", result)
		}
	}
	for _, bad := range []string{"null", "[]", "{broken"} {
		if _, err := prepareInferencePayload([]byte(bad), "backup", false); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDeepSeekHistoryPreservesReasoningAndAcceptsFailoverHistory(t *testing.T) {
	body := []byte(`{"model":"live-model","messages":[{"role":"user","content":"help"},{"role":"assistant","content":"answer","reasoning_content":"original"},{"role":"assistant","tool_calls":[{"id":"one"}]},{"role":"tool","tool_call_id":"one","content":"result"}],"tools":[{"type":"function"}]}`)
	result, err := prepareDeepSeekHistory(body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatal(err)
	}
	messages := payload["messages"].([]any)
	if messages[1].(map[string]any)["reasoning_content"] != "original" {
		t.Fatal("lost original reasoning")
	}
	if reasoning, ok := messages[2].(map[string]any)["reasoning_content"]; !ok || reasoning != "" {
		t.Fatal("missing empty reasoning field for non-DeepSeek history")
	}
	if _, ok := messages[0].(map[string]any)["reasoning_content"]; ok {
		t.Fatal("added reasoning to user")
	}
	if payload["tools"] == nil || messages[2].(map[string]any)["tool_calls"] == nil {
		t.Fatal("lost tool history")
	}
}
