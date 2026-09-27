package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Claude-compatible API-key thinking modes configured per model
// (claude-api-key[].models[].thinking-mode).
const (
	ClaudeThinkingModePassthrough = "passthrough"
	ClaudeThinkingModeDisabled    = "disabled"
	ClaudeThinkingModeCap         = "cap"
)

// claudeEffortOrder ranks output_config.effort values from least to most thinking.
var claudeEffortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ClaudeThinkingPolicy is the resolved per-model thinking override for a
// claude-api-key upstream. The zero value is inactive and keeps the default
// thinking pipeline output byte-for-byte.
type ClaudeThinkingPolicy struct {
	Mode      string
	EffortCap string
}

// Active reports whether the policy changes the request body.
func (p ClaudeThinkingPolicy) Active() bool {
	return p.Mode != ""
}

// ResolveClaudeThinkingPolicy returns the thinking override configured on the
// claude-api-key model entry whose name or alias matches one of models.
// Unknown modes and a "cap" mode without a valid effort level are ignored or
// degraded to passthrough with a warning, never rejected.
func ResolveClaudeThinkingPolicy(entry *config.ClaudeKey, models ...string) ClaudeThinkingPolicy {
	if entry == nil || len(entry.Models) == 0 {
		return ClaudeThinkingPolicy{}
	}
	for i := range entry.Models {
		model := &entry.Models[i]
		if !claudeModelEntryMatches(model, models) {
			continue
		}
		return normalizeClaudeThinkingPolicy(model)
	}
	return ClaudeThinkingPolicy{}
}

func claudeModelEntryMatches(model *config.ClaudeModel, candidates []string) bool {
	name := strings.TrimSpace(model.Name)
	alias := strings.TrimSpace(model.Alias)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if (name != "" && strings.EqualFold(name, candidate)) || (alias != "" && strings.EqualFold(alias, candidate)) {
			return true
		}
	}
	return false
}

func normalizeClaudeThinkingPolicy(model *config.ClaudeModel) ClaudeThinkingPolicy {
	mode := strings.ToLower(strings.TrimSpace(model.ThinkingMode))
	effortCap := strings.ToLower(strings.TrimSpace(model.ThinkingEffortCap))
	validCap := effortCap != "" && effortCap != "none" && claudeEffortRank(effortCap) >= 0
	if effortCap != "" && !validCap {
		log.WithFields(log.Fields{
			"model":               model.Name,
			"thinking_effort_cap": model.ThinkingEffortCap,
		}).Warn("claude thinking policy: invalid thinking-effort-cap, ignored |")
		effortCap = ""
	}
	switch mode {
	case "":
		if validCap {
			return ClaudeThinkingPolicy{Mode: ClaudeThinkingModeCap, EffortCap: effortCap}
		}
		return ClaudeThinkingPolicy{}
	case ClaudeThinkingModePassthrough, ClaudeThinkingModeDisabled:
		return ClaudeThinkingPolicy{Mode: mode}
	case ClaudeThinkingModeCap:
		if !validCap {
			log.WithField("model", model.Name).Warn("claude thinking policy: cap mode without a valid thinking-effort-cap, using passthrough |")
			return ClaudeThinkingPolicy{Mode: ClaudeThinkingModePassthrough}
		}
		return ClaudeThinkingPolicy{Mode: ClaudeThinkingModeCap, EffortCap: effortCap}
	default:
		log.WithFields(log.Fields{
			"model":         model.Name,
			"thinking_mode": model.ThinkingMode,
		}).Warn("claude thinking policy: unknown thinking-mode, ignored |")
		return ClaudeThinkingPolicy{}
	}
}

// ApplyClaudeThinkingPolicy rewrites the thinking fields of processed (the
// body after the canonical thinking pipeline) according to policy. source is
// the Claude-format body captured before the thinking pipeline ran; it carries
// the client's thinking intent that the pipeline may have stripped for models
// without a declared thinking capability.
func ApplyClaudeThinkingPolicy(source, processed []byte, policy ClaudeThinkingPolicy) []byte {
	switch policy.Mode {
	case ClaudeThinkingModeDisabled:
		out, _ := sjson.SetRawBytes(processed, "thinking", []byte(`{"type":"disabled"}`))
		return deleteClaudeEffort(out)
	case ClaudeThinkingModePassthrough:
		return restoreClaudeThinkingFields(source, processed)
	case ClaudeThinkingModeCap:
		return capClaudeThinkingEffort(restoreClaudeThinkingFields(source, processed), policy.EffortCap)
	default:
		return processed
	}
}

func restoreClaudeThinkingFields(source, processed []byte) []byte {
	out := processed
	if thinkingField := gjson.GetBytes(source, "thinking"); thinkingField.Exists() {
		out, _ = sjson.SetRawBytes(out, "thinking", []byte(thinkingField.Raw))
	} else {
		out, _ = sjson.DeleteBytes(out, "thinking")
	}
	if effort := gjson.GetBytes(source, "output_config.effort"); effort.Exists() {
		out, _ = sjson.SetRawBytes(out, "output_config.effort", []byte(effort.Raw))
		return out
	}
	return deleteClaudeEffort(out)
}

func capClaudeThinkingEffort(body []byte, effortCap string) []byte {
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "adaptive", "enabled", "auto":
	default:
		// Disabled or absent thinking is already at or below any cap.
		return body
	}
	current := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String()))
	if rank := claudeEffortRank(current); rank >= 0 && rank <= claudeEffortRank(effortCap) {
		return body
	}
	// A missing or unknown effort means the upstream default (full thinking).
	out, _ := sjson.SetBytes(body, "output_config.effort", effortCap)
	return out
}

func deleteClaudeEffort(body []byte) []byte {
	out, _ := sjson.DeleteBytes(body, "output_config.effort")
	if oc := gjson.GetBytes(out, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
		out, _ = sjson.DeleteBytes(out, "output_config")
	}
	return out
}

func claudeEffortRank(level string) int {
	for i, candidate := range claudeEffortOrder {
		if candidate == level {
			return i
		}
	}
	return -1
}
