// Package deepseek implements thinking configuration for DeepSeek OpenAI-compatible models.
package deepseek

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Applier implements thinking.ProviderApplier for DeepSeek models.
//
// DeepSeek's Chat Completions compatibility uses thinking.type to enable/disable
// reasoning and accepts a compact reasoning_effort vocabulary where xhigh/max map
// to max, while other enabled efforts map to high.
type Applier struct{}

var _ thinking.ProviderApplier = (*Applier)(nil)

// NewApplier creates a new DeepSeek thinking applier.
func NewApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("deepseek", NewApplier())
}

// Apply applies thinking configuration to DeepSeek request body.
func (a *Applier) Apply(body []byte, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) ([]byte, error) {
	if modelInfo != nil && modelInfo.Thinking == nil && !thinking.IsUserDefinedModel(modelInfo) {
		return body, nil
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		body = []byte(`{}`)
	}

	switch config.Mode {
	case thinking.ModeNone:
		return applyThinkingType(body, "disabled", "")
	case thinking.ModeLevel:
		return applyThinkingType(body, "enabled", mapDeepSeekEffort(string(config.Level)))
	case thinking.ModeAuto:
		return applyThinkingType(body, "enabled", "high")
	case thinking.ModeBudget:
		level, ok := thinking.ConvertBudgetToLevel(config.Budget)
		if !ok {
			return body, nil
		}
		if strings.EqualFold(level, string(thinking.LevelNone)) {
			return applyThinkingType(body, "disabled", "")
		}
		return applyThinkingType(body, "enabled", mapDeepSeekEffort(level))
	default:
		return body, nil
	}
}

func mapDeepSeekEffort(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "xhigh", "max":
		return "max"
	case "", "none", "off", "disabled":
		return ""
	default:
		return "high"
	}
}

func applyThinkingType(body []byte, thinkingType string, effort string) ([]byte, error) {
	result, err := sjson.DeleteBytes(body, "thinking")
	if err != nil {
		return body, fmt.Errorf("deepseek thinking: failed to clear thinking object: %w", err)
	}
	result, err = sjson.DeleteBytes(result, "reasoning_effort")
	if err != nil {
		return body, fmt.Errorf("deepseek thinking: failed to clear reasoning_effort: %w", err)
	}
	result, err = sjson.SetBytes(result, "thinking.type", thinkingType)
	if err != nil {
		return body, fmt.Errorf("deepseek thinking: failed to set thinking.type: %w", err)
	}
	if effort == "" {
		return result, nil
	}
	result, err = sjson.SetBytes(result, "reasoning_effort", effort)
	if err != nil {
		return body, fmt.Errorf("deepseek thinking: failed to set reasoning_effort: %w", err)
	}
	return result, nil
}
