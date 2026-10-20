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
