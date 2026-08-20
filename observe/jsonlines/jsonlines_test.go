package jsonlines_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/observe/jsonlines"
)

func TestModuleContributesAllObservationComponents(t *testing.T) {
	module, err := jsonlines.NewModule("observe.json.test", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	builder := agentslot.NewBuilder()
	if err := builder.Install(module); err != nil {
		t.Fatal(err)
	}
	assembly, err := builder.Build(
		agentslot.RequireChain(observe.TraceSlot, 1), agentslot.RequireChain(observe.MetricSlot, 1),
		agentslot.RequireChain(observe.AuditSlot, 1), agentslot.RequireChain(observe.UsageSlot, 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentslot.Ordered(assembly, observe.TraceSlot)) != 1 || len(agentslot.Ordered(assembly, observe.UsageSlot)) != 1 {
		t.Fatal("JSON Lines observation components missing")
	}
}

func TestRecorderWritesConcurrentRecordsAsCompleteJSONLines(t *testing.T) {
	var output bytes.Buffer
	recorder, err := jsonlines.New(&output)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	identity := observe.Identity{
		SessionID: "session-1", RunID: "run-1", StepID: "step-1", ToolCallID: "call-1", AttemptID: "a",
		Actor: agent.ActorIdentity{Kind: agent.ActorService, ID: "test"},
	}
	operations := []func() error{
		func() error {
			return recorder.RecordTrace(context.Background(), observe.TraceRecord{Kind: observe.TraceRunStarted, At: now, Identity: identity})
		},
		func() error {
			return recorder.RecordMetric(context.Background(), observe.MetricRecord{Name: observe.MetricRunTotal, Kind: observe.MetricCounter, Value: 1, At: now, Identity: identity})
		},
		func() error {
			return recorder.RecordAudit(context.Background(), observe.AuditRecord{Kind: observe.AuditToolDecision, At: now, Identity: identity, Action: "bash", Decision: "allow"})
		},
		func() error {
			return recorder.RecordUsage(context.Background(), observe.UsageRecord{Kind: observe.UsageModel, At: now, Identity: identity, ProviderKey: "p", ModelID: "m", TotalTokens: 1})
		},
	}
	var wait sync.WaitGroup
	for _, operation := range operations {
		wait.Add(1)
		go func(operation func() error) {
			defer wait.Done()
			if err := operation(); err != nil {
				t.Errorf("record: %v", err)
			}
		}(operation)
	}
	wait.Wait()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %q", output.String())
	}
	kinds := make(map[string]bool)
	for _, line := range lines {
		var envelope struct {
			Type   string          `json:"type"`
			Record json.RawMessage `json:"record"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil || envelope.Type == "" || !json.Valid(envelope.Record) {
			t.Fatalf("invalid JSON line %q: %#v, %v", line, envelope, err)
		}
		kinds[envelope.Type] = true
	}
	for _, kind := range []string{"trace", "metric", "audit", "usage"} {
		if !kinds[kind] {
			t.Fatalf("missing %q envelope in %q", kind, output.String())
		}
	}
}

func TestRecorderRejectsNilWriterAndInvalidRecord(t *testing.T) {
	if _, err := jsonlines.New(nil); err == nil {
		t.Fatal("nil writer accepted")
	}
	recorder, err := jsonlines.New(&bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordTrace(context.Background(), observe.TraceRecord{}); err == nil {
		t.Fatal("invalid trace record accepted")
	}
}
