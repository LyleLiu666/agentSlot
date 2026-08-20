package model_test

import (
	"testing"

	"github.com/LyleLiu666/agentSlot/model"
)

func TestTokenUsageValidatesSubsetsAndEstimates(t *testing.T) {
	valid := model.TokenUsage{
		InputTokens: 100, OutputTokens: 40, CachedInputTokens: 60,
		CacheWriteTokens: 20, ReasoningTokens: 10, TotalTokens: 140,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid usage: %v", err)
	}
	invalid := valid
	invalid.CachedInputTokens = 101
	if err := invalid.Validate(); err == nil {
		t.Fatal("cached input greater than input was accepted")
	}
	estimated := valid
	estimated.Estimated = true
	if err := estimated.Validate(); err == nil {
		t.Fatal("estimate without source was accepted")
	}
	estimated.EstimateSource = "local-tokenizer"
	if err := estimated.Validate(); err != nil {
		t.Fatalf("estimated usage: %v", err)
	}
}
