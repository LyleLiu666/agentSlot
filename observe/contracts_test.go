package observe_test

import (
	"context"
	"math"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/observe"
)

func TestObservationComponentsAreIndependentOrderedChains(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(observationModule{}); err != nil {
		t.Fatal(err)
	}
	assembly, err := builder.Build(
		agentslot.RequireChain(observe.TraceSlot, 1),
		agentslot.RequireChain(observe.MetricSlot, 1),
		agentslot.RequireChain(observe.AuditSlot, 1),
		agentslot.RequireChain(observe.UsageSlot, 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentslot.Ordered(assembly, observe.TraceSlot)) != 1 ||
		len(agentslot.Ordered(assembly, observe.MetricSlot)) != 1 ||
		len(agentslot.Ordered(assembly, observe.AuditSlot)) != 1 ||
		len(agentslot.Ordered(assembly, observe.UsageSlot)) != 1 {
		t.Fatal("one or more observation chains were not assembled")
	}
}

func TestObservationRecordsValidatePortableFacts(t *testing.T) {
	identity := observe.Identity{
		SessionID: "session-1", RunID: "run-1", StepID: "step-1", ToolCallID: "call-1", AttemptID: "attempt-1",
		Actor: agent.ActorIdentity{Kind: agent.ActorService, ID: "test"},
	}
	now := time.Now().UTC()
	valid := []interface{ Validate() error }{
		observe.TraceRecord{Kind: observe.TraceRunStarted, At: now, Identity: identity},
		observe.MetricRecord{Name: observe.MetricRunTotal, Kind: observe.MetricCounter, Value: 1, At: now, Identity: identity, Attributes: map[string]string{"outcome": "started"}},
		observe.AuditRecord{Kind: observe.AuditToolDecision, At: now, Identity: identity, Action: "bash", Decision: "allow"},
		observe.UsageRecord{Kind: observe.UsageModel, At: now, Identity: identity, ProviderKey: "provider", ModelID: "model", InputTokens: 2, OutputTokens: 3, CachedInputTokens: 1, CacheWriteTokens: 1, ReasoningTokens: 2, TotalTokens: 5},
	}
	for _, record := range valid {
		if err := record.Validate(); err != nil {
			t.Fatalf("valid record %T rejected: %v", record, err)
		}
	}
	withoutProvider := observe.UsageRecord{
		Kind: observe.UsageModel, At: now, Identity: identity, ModelID: "executor-owned-model", TotalTokens: 1,
	}
	if err := withoutProvider.Validate(); err != nil {
		t.Fatalf("usage for an Executor-owned Provider was rejected: %v", err)
	}
	if err := (observe.MetricRecord{Name: observe.MetricRunTotal, Kind: observe.MetricCounter, Value: math.NaN(), At: now}).Validate(); err == nil {
		t.Fatal("NaN metric accepted")
	}
	if err := (observe.UsageRecord{Kind: observe.UsageModel, At: now, Identity: identity, ProviderKey: "p", ModelID: "m", InputTokens: 2, OutputTokens: 3, TotalTokens: 4}).Validate(); err == nil {
		t.Fatal("inconsistent token total accepted")
	}
	if err := (observe.UsageRecord{Kind: observe.UsageModel, At: now, Identity: identity, ProviderKey: "p", ModelID: "m", InputTokens: 2, CachedInputTokens: 3, TotalTokens: 2}).Validate(); err == nil {
		t.Fatal("cached input larger than input was accepted")
	}
}

func TestObservationRecordsRequireIdentityForTheirSpecificFactKind(t *testing.T) {
	now := time.Now().UTC()
	invalid := []interface{ Validate() error }{
		observe.TraceRecord{Kind: observe.TraceRunStarted, At: now, Identity: observe.Identity{SessionID: "session-1"}},
		observe.TraceRecord{Kind: observe.TraceModelAttemptStarted, At: now, Identity: observe.Identity{SessionID: "session-1", RunID: "run-1", AttemptID: "attempt-1"}},
		observe.TraceRecord{Kind: observe.TraceToolStarted, At: now, Identity: observe.Identity{SessionID: "session-1", RunID: "run-1", StepID: "step-1"}},
		observe.AuditRecord{Kind: observe.AuditToolDecision, At: now, Identity: observe.Identity{SessionID: "session-1"}, Action: "bash", Decision: "allow"},
	}
	for _, record := range invalid {
		if err := record.Validate(); err == nil {
			t.Fatalf("incomplete record %T accepted: %#v", record, record)
		}
	}
}

type observationModule struct{}

func (observationModule) ID() string { return "observe.contracts" }
func (observationModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Append(observe.TraceSlot, observe.TraceSink(observe.TraceFunc(func(context.Context, observe.TraceRecord) error { return nil }))),
		agentslot.Append(observe.MetricSlot, observe.MetricSink(observe.MetricFunc(func(context.Context, observe.MetricRecord) error { return nil }))),
		agentslot.Append(observe.AuditSlot, observe.AuditSink(observe.AuditFunc(func(context.Context, observe.AuditRecord) error { return nil }))),
		agentslot.Append(observe.UsageSlot, observe.UsageRecorder(observe.UsageFunc(func(context.Context, observe.UsageRecord) error { return nil }))),
	)
}
