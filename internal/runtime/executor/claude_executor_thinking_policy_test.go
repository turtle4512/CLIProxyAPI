package executor

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelconfig"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// Claude Code 2.1.x request shape for a GLM model: adaptive thinking plus effort.
const glmPolicyPayload = `{"model":"glm-5.3-flash","max_tokens":32000,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`

func glmPolicyUpstream(t *testing.T, seen *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*seen = bytes.Clone(body)
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"glm-5.3-flash\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"glm-5.3-flash","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
}

// glmPolicyRun sends the payload through the executor the way the conductor
// does for a claude-api-key model: the resolved model info has no thinking
// capability and is not user-defined.
func glmPolicyRun(t *testing.T, models []config.ClaudeModel, stream bool) []byte {
	t.Helper()
	var seen []byte
	server := glmPolicyUpstream(t, &seen)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{ClaudeKey: []config.ClaudeKey{
		{APIKey: "key-other", BaseURL: server.URL, Models: []config.ClaudeModel{{Name: "glm-5.3-flash", Alias: "glm-5.3-flash", ThinkingMode: "disabled"}}},
		{APIKey: "key-glm", BaseURL: server.URL, Models: models},
	}})
	auth := &cliproxyauth.Auth{
		ID:       "glm-auth",
		Provider: "claude",
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey:   "key-glm",
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindAPIKey,
			"base_url":                     server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "glm-5.3-flash",
		Payload: []byte(glmPolicyPayload),
		Metadata: map[string]any{
			"cliproxy.resolved_api_key_model_info": modelconfig.ResolveModelInfo("glm-5.3-flash", "claude", nil),
		},
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	if stream {
		result, err := executor.ExecuteStream(contextWithGinHeaders(map[string]string{"User-Agent": "claude-cli/2.1.283 (external, cli)", "X-App": "cli"}), auth, req, opts)
		if err != nil {
			t.Fatalf("ExecuteStream() error = %v", err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
		}
	} else {
		if _, err := executor.Execute(contextWithGinHeaders(map[string]string{"User-Agent": "claude-cli/2.1.283 (external, cli)", "X-App": "cli"}), auth, req, opts); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	}
	if len(seen) == 0 {
		t.Fatal("upstream body not captured")
	}
	return seen
}

func TestClaudeExecutorThinkingPolicy_DefaultStripsAsBefore(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body := glmPolicyRun(t, []config.ClaudeModel{{Name: "glm-5.3-flash", Alias: "glm-5.3-flash"}}, stream)
		if gjson.GetBytes(body, "thinking").Exists() || gjson.GetBytes(body, "output_config.effort").Exists() {
			t.Fatalf("stream=%v: default config must keep stripping thinking fields: %s", stream, body)
		}
	}
}

func TestClaudeExecutorThinkingPolicy_CapLowersEffort(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body := glmPolicyRun(t, []config.ClaudeModel{{Name: "glm-5.3-flash", Alias: "glm-5.3-flash", ThinkingEffortCap: "low"}}, stream)
		if got := gjson.GetBytes(body, "thinking.type").String(); got != "adaptive" {
			t.Fatalf("stream=%v: thinking.type = %q, want adaptive (%s)", stream, got, body)
		}
		if got := gjson.GetBytes(body, "output_config.effort").String(); got != "low" {
			t.Fatalf("stream=%v: effort = %q, want low (%s)", stream, got, body)
		}
	}
}

func TestClaudeExecutorThinkingPolicy_PassthroughAndDisabled(t *testing.T) {
	body := glmPolicyRun(t, []config.ClaudeModel{{Name: "glm-5.3-flash", Alias: "glm-5.3-flash", ThinkingMode: "passthrough"}}, true)
	if gjson.GetBytes(body, "thinking.type").String() != "adaptive" || gjson.GetBytes(body, "output_config.effort").String() != "high" {
		t.Fatalf("passthrough must forward client thinking fields: %s", body)
	}
	body = glmPolicyRun(t, []config.ClaudeModel{{Name: "glm-5.3-flash", Alias: "glm-5.3-flash", ThinkingMode: "disabled"}}, true)
	if gjson.GetBytes(body, "thinking.type").String() != "disabled" || gjson.GetBytes(body, "output_config.effort").Exists() {
		t.Fatalf("disabled must force thinking off: %s", body)
	}
}

// A policy on one claude-api-key entry must not leak to another credential or
// to models of the same entry that do not configure it.
func TestClaudeExecutorThinkingPolicy_ScopedToEntryAndModel(t *testing.T) {
	body := glmPolicyRun(t, []config.ClaudeModel{
		{Name: "glm-5.3-flash", Alias: "glm-5.3-flash"},
		{Name: "glm-5.3", Alias: "glm-5.3", ThinkingMode: "disabled"},
	}, true)
	if gjson.GetBytes(body, "thinking").Exists() {
		t.Fatalf("policy leaked from sibling model or other credential: %s", body)
	}
	if !strings.Contains(string(body), `"messages"`) {
		t.Fatalf("unexpected upstream body: %s", body)
	}
}
