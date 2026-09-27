package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestResolveClaudeThinkingPolicy(t *testing.T) {
	entry := &config.ClaudeKey{Models: []config.ClaudeModel{
		{Name: "glm-plain", Alias: "glm-plain"},
		{Name: "glm-cap", Alias: "glm-cap-alias", ThinkingEffortCap: " LOW "},
		{Name: "glm-off", ThinkingMode: "Disabled"},
		{Name: "glm-pass", ThinkingMode: "passthrough"},
		{Name: "glm-cap-invalid", ThinkingMode: "cap", ThinkingEffortCap: "tiny"},
		{Name: "glm-unknown", ThinkingMode: "turbo"},
		{Name: "glm-cap-none", ThinkingEffortCap: "none"},
	}}
	tests := []struct {
		name   string
		entry  *config.ClaudeKey
		models []string
		want   ClaudeThinkingPolicy
	}{
		{name: "nil entry", entry: nil, models: []string{"glm-cap"}, want: ClaudeThinkingPolicy{}},
		{name: "unconfigured model keeps default", entry: entry, models: []string{"glm-plain"}, want: ClaudeThinkingPolicy{}},
		{name: "model not listed", entry: entry, models: []string{"claude-opus"}, want: ClaudeThinkingPolicy{}},
		{name: "cap implied by effort cap", entry: entry, models: []string{"glm-cap"}, want: ClaudeThinkingPolicy{Mode: ClaudeThinkingModeCap, EffortCap: "low"}},
		{name: "matches alias", entry: entry, models: []string{"", "GLM-CAP-ALIAS"}, want: ClaudeThinkingPolicy{Mode: ClaudeThinkingModeCap, EffortCap: "low"}},
		{name: "disabled", entry: entry, models: []string{"glm-off"}, want: ClaudeThinkingPolicy{Mode: ClaudeThinkingModeDisabled}},
		{name: "passthrough", entry: entry, models: []string{"glm-pass"}, want: ClaudeThinkingPolicy{Mode: ClaudeThinkingModePassthrough}},
		{name: "cap with invalid level degrades to passthrough", entry: entry, models: []string{"glm-cap-invalid"}, want: ClaudeThinkingPolicy{Mode: ClaudeThinkingModePassthrough}},
		{name: "unknown mode ignored", entry: entry, models: []string{"glm-unknown"}, want: ClaudeThinkingPolicy{}},
		{name: "none is not a cap", entry: entry, models: []string{"glm-cap-none"}, want: ClaudeThinkingPolicy{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveClaudeThinkingPolicy(tt.entry, tt.models...)
			if got != tt.want {
				t.Fatalf("ResolveClaudeThinkingPolicy() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Claude Code sends adaptive thinking with output_config.effort. The default
// pipeline strips both for a model without declared thinking support; processed
// mirrors that stripped body.
const (
	ccSourceBody    = `{"model":"glm-5.3-flash","max_tokens":32000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[]}`
	ccStrippedBody  = `{"model":"glm-5.3-flash","max_tokens":32000,"messages":[]}`
	ccKeepFormatSrc = `{"model":"m","thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh","format":{"type":"json_schema"}},"messages":[]}`
	ccKeepFormatOut = `{"model":"m","output_config":{"format":{"type":"json_schema"}},"messages":[]}`
)

func TestApplyClaudeThinkingPolicy_InactiveIsIdentity(t *testing.T) {
	got := ApplyClaudeThinkingPolicy([]byte(ccSourceBody), []byte(ccStrippedBody), ClaudeThinkingPolicy{})
	if string(got) != ccStrippedBody {
		t.Fatalf("inactive policy changed body: %s", got)
	}
}

func TestApplyClaudeThinkingPolicy_Passthrough(t *testing.T) {
	got := ApplyClaudeThinkingPolicy([]byte(ccSourceBody), []byte(ccStrippedBody), ClaudeThinkingPolicy{Mode: ClaudeThinkingModePassthrough})
	if raw := gjson.GetBytes(got, "thinking").Raw; raw != `{"type":"adaptive"}` {
		t.Fatalf("thinking = %s, want client value", raw)
	}
	if effort := gjson.GetBytes(got, "output_config.effort").String(); effort != "high" {
		t.Fatalf("effort = %q, want high", effort)
	}

	disabledSrc := `{"thinking":{"type":"disabled"},"messages":[]}`
	got = ApplyClaudeThinkingPolicy([]byte(disabledSrc), []byte(`{"messages":[]}`), ClaudeThinkingPolicy{Mode: ClaudeThinkingModePassthrough})
	if typ := gjson.GetBytes(got, "thinking.type").String(); typ != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled forwarded", typ)
	}

	// A field the client never sent must not appear.
	got = ApplyClaudeThinkingPolicy([]byte(`{"messages":[]}`), []byte(`{"thinking":{"type":"enabled"},"output_config":{"effort":"low"},"messages":[]}`), ClaudeThinkingPolicy{Mode: ClaudeThinkingModePassthrough})
	if gjson.GetBytes(got, "thinking").Exists() || gjson.GetBytes(got, "output_config").Exists() {
		t.Fatalf("passthrough invented thinking fields: %s", got)
	}
}

func TestApplyClaudeThinkingPolicy_Disabled(t *testing.T) {
	got := ApplyClaudeThinkingPolicy([]byte(ccSourceBody), []byte(ccSourceBody), ClaudeThinkingPolicy{Mode: ClaudeThinkingModeDisabled})
	if raw := gjson.GetBytes(got, "thinking").Raw; raw != `{"type":"disabled"}` {
		t.Fatalf("thinking = %s, want disabled", raw)
	}
	if gjson.GetBytes(got, "output_config").Exists() {
		t.Fatalf("output_config should be removed when empty: %s", got)
	}
	got = ApplyClaudeThinkingPolicy([]byte(ccKeepFormatSrc), []byte(ccKeepFormatOut), ClaudeThinkingPolicy{Mode: ClaudeThinkingModeDisabled})
	if !gjson.GetBytes(got, "output_config.format").Exists() || gjson.GetBytes(got, "output_config.effort").Exists() {
		t.Fatalf("disabled must drop only effort: %s", got)
	}
}

func TestApplyClaudeThinkingPolicy_Cap(t *testing.T) {
	low := ClaudeThinkingPolicy{Mode: ClaudeThinkingModeCap, EffortCap: "low"}
	tests := []struct {
		name       string
		source     string
		processed  string
		wantType   string
		wantEffort string
		wantBudget int64
	}{
		{name: "claude code adaptive high is capped", source: ccSourceBody, processed: ccStrippedBody, wantType: "adaptive", wantEffort: "low"},
		{name: "lower effort kept", source: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"minimal"}}`, processed: `{}`, wantType: "adaptive", wantEffort: "minimal"},
		{name: "equal effort kept", source: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`, processed: `{}`, wantType: "adaptive", wantEffort: "low"},
		{name: "missing effort filled", source: `{"thinking":{"type":"adaptive"}}`, processed: `{}`, wantType: "adaptive", wantEffort: "low"},
		{name: "unknown effort replaced", source: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"ultra"}}`, processed: `{}`, wantType: "adaptive", wantEffort: "low"},
		{name: "enabled budget gets effort cap", source: `{"thinking":{"type":"enabled","budget_tokens":8000}}`, processed: `{}`, wantType: "enabled", wantEffort: "low", wantBudget: 8000},
		{name: "client disabled untouched", source: `{"thinking":{"type":"disabled"}}`, processed: `{}`, wantType: "disabled"},
		{name: "absent thinking untouched", source: `{"messages":[]}`, processed: `{"messages":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyClaudeThinkingPolicy([]byte(tt.source), []byte(tt.processed), low)
			if typ := gjson.GetBytes(got, "thinking.type").String(); typ != tt.wantType {
				t.Fatalf("thinking.type = %q, want %q (%s)", typ, tt.wantType, got)
			}
			if effort := gjson.GetBytes(got, "output_config.effort").String(); effort != tt.wantEffort {
				t.Fatalf("effort = %q, want %q (%s)", effort, tt.wantEffort, got)
			}
			if budget := gjson.GetBytes(got, "thinking.budget_tokens").Int(); budget != tt.wantBudget {
				t.Fatalf("budget_tokens = %d, want %d (%s)", budget, tt.wantBudget, got)
			}
		})
	}

	got := ApplyClaudeThinkingPolicy([]byte(ccKeepFormatSrc), []byte(ccKeepFormatOut), low)
	if gjson.GetBytes(got, "output_config.format.type").String() != "json_schema" || gjson.GetBytes(got, "output_config.effort").String() != "low" {
		t.Fatalf("cap must keep other output_config fields: %s", got)
	}
}
