package standardagent

import (
	"context"
	"sync"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/policy"
	"github.com/LyleLiu666/agentSlot/session"
)

func TestRuntimePublishesPassiveTraceMetricAuditAndUsageFacts(t *testing.T) {
	records := &observationRecords{changed: make(chan struct{}, 64)}
	fake := model.NewFakeModelExecutor(model.FakeExecution{Usage: model.TokenUsage{
		InputTokens: 4, OutputTokens: 2, CachedInputTokens: 3, CacheWriteTokens: 1, ReasoningTokens: 1, TotalTokens: 6,
	}, Events: []model.ModelEvent{
		{Kind: model.EventComplete, AttemptID: "attempt-1", Output: &model.Completion{Parts: textInput("done").Parts}},
	}})
	access, stop := startObservedApplication(t, fake, records, AgentRuntimeConfig{})
	opened := createRuntimeTestSession(t, access)
	user := agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "user-1"}
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision, Actor: user, Input: textInput("hello"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	snapshot, err := access.View(context.Background(), interaction.SessionViewRequest{SessionID: opened.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := access.UpdateModelConfig(context.Background(), interaction.UpdateModelConfigRequest{
		SessionID: opened.SessionID, ExpectedRevision: snapshot.Revision, Actor: user,
		Config: model.Config{ProviderKey: "provider", ModelID: "second", Reasoning: model.ReasoningDefault},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.CloseSession(context.Background(), interaction.CloseSessionRequest{SessionID: opened.SessionID, ExpectedRevision: updated.Revision, Actor: user}); err != nil {
		t.Fatal(err)
	}
	stop()

	if !records.hasTrace(observe.TraceRuntimeOpened) || !records.hasTrace(observe.TraceRunStarted) ||
		!records.hasTrace(observe.TraceModelAttemptStarted) || !records.hasTrace(observe.TraceModelAttemptDone) ||
		!records.hasTrace(observe.TraceRunCompleted) || !records.hasTrace(observe.TraceRuntimeClosed) {
		t.Fatalf("trace records = %#v", records.tracesCopy())
	}
	if !records.hasMetric(observe.MetricRunTotal) || !records.hasMetric(observe.MetricModelAttemptTotal) {
		t.Fatalf("metric records = %#v", records.metricsCopy())
	}
	usage := records.usageCopy()
	if len(usage) != 1 || usage[0].Identity.AttemptID != "attempt-1" || !usage[0].Identity.Actor.Valid() ||
		usage[0].InputTokens != 4 || usage[0].OutputTokens != 2 || usage[0].CachedInputTokens != 3 ||
		usage[0].CacheWriteTokens != 1 || usage[0].ReasoningTokens != 1 || usage[0].ProviderKey != "provider" {
		t.Fatalf("usage records = %#v", usage)
	}
	audits := records.auditsCopy()
	if len(audits) != 1 || audits[0].Kind != observe.AuditModelConfigChanged || audits[0].Action != "provider/second" || audits[0].Decision != "committed" || audits[0].Identity.Actor != user {
		t.Fatalf("audit records = %#v", audits)
	}
}

func TestToolPolicyAndExecutionPublishAuditTraceAndMetricWithoutGivingSinksControl(t *testing.T) {
	records := &observationRecords{changed: make(chan struct{}, 64)}
	installed := &countingTool{definition: testToolDefinition(t, "effect")}
	fake := model.NewFakeModelExecutor(
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, AttemptID: "attempt-1", Output: &model.Completion{
			ToolCalls: []model.ToolCallRequest{{Name: "effect", Arguments: []byte(`{"value":"hello"}`)}},
		}}}},
		model.FakeExecution{Events: []model.ModelEvent{{Kind: model.EventComplete, AttemptID: "attempt-2", Output: &model.Completion{Parts: textInput("done").Parts}}}},
	)
	guard := policy.GuardFunc(func(context.Context, policy.Action) (policy.Decision, error) {
		return policy.Decision{Effect: policy.RequireApproval, Reason: "external effect"}, nil
	})
	approval := policy.ApprovalFunc(func(context.Context, policy.ApprovalRequest) (policy.ApprovalDecision, error) {
		return policy.ApprovalDecision{Approved: true}, nil
	})
	access, stop := startObservedApplication(t, fake, records, AgentRuntimeConfig{ToolKeys: []string{"effect"}},
		toolModule{key: "effect", value: installed}, policyModule{guard: guard, approval: approval},
	)
	opened := createRuntimeTestSession(t, access)
	if _, err := access.Send(context.Background(), interaction.SendRequest{
		SessionID: opened.SessionID, ExpectedRevision: opened.Revision,
		Actor: agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "user-1"}, Input: textInput("use tool"),
	}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeIdle(t, access, opened.SessionID)
	stop()
	if installed.calls.Load() != 1 {
		t.Fatalf("tool calls = %d", installed.calls.Load())
	}
	if !records.hasTrace(observe.TraceToolStarted) || !records.hasTrace(observe.TraceToolCompleted) || !records.hasMetric(observe.MetricToolCallTotal) {
		t.Fatalf("tool observations = traces %#v metrics %#v", records.tracesCopy(), records.metricsCopy())
	}
	audits := records.auditsCopy()
	found := false
	for _, audit := range audits {
		if audit.Kind == observe.AuditToolDecision && audit.Action == "effect" && audit.Decision == "approved" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool audits = %#v", audits)
	}
}

func startObservedApplication(t *testing.T, executor model.ModelExecutor, records *observationRecords, runtimeConfig AgentRuntimeConfig, extras ...agentslot.Module) (interaction.GatewayAccess, func()) {
	t.Helper()
	if len(runtimeConfig.ToolKeys) > 0 && runtimeConfig.MaxInlineToolResultBytes == 0 {
		runtimeConfig.MaxInlineToolResultBytes = testMaxInlineToolResultBytes
	}
	defaultModel := model.Config{ProviderKey: "provider", ModelID: "first", Reasoning: model.ReasoningDefault}
	memory := session.NewMemoryModule()
	entry := &captureChannel{}
	modules := []agentslot.Module{memory, executorModule{executor: executor}, observationModule{records: records}}
	modules = append(modules, extras...)
	modules = append(modules, NewGatewayChannelModule("entrypoint.observation-test", "test", entry))
	application := NewApplication(ApplicationSpec{Name: "observation-test", Modules: modules, RuntimeConfig: runtimeConfig, DefaultModelConfig: defaultModel})
	running, err := application.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return entry.Access(), func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := running.Stop(ctx); err != nil {
				t.Errorf("stop: %v", err)
			}
		})
	}
}

type observationModule struct{ records *observationRecords }

func (observationModule) ID() string { return "test.observations" }
func (m observationModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		// A broken optional sink must not prevent later sinks or Runtime work.
		agentslot.Append(observe.TraceSlot, observe.TraceSink(observe.TraceFunc(func(context.Context, observe.TraceRecord) error { panic("broken trace sink") }))),
		agentslot.Append(observe.TraceSlot, observe.TraceSink(observe.TraceFunc(m.records.trace))),
		agentslot.Append(observe.MetricSlot, observe.MetricSink(observe.MetricFunc(m.records.metric))),
		agentslot.Append(observe.AuditSlot, observe.AuditSink(observe.AuditFunc(m.records.audit))),
		agentslot.Append(observe.UsageSlot, observe.UsageRecorder(observe.UsageFunc(m.records.usage))),
	)
}

type policyModule struct {
	guard    policy.PolicyGuard
	approval policy.ApprovalService
}

func (policyModule) ID() string { return "test.policy" }
func (m policyModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Append(policy.GuardSlot, m.guard),
		agentslot.Set(policy.ApprovalSlot, m.approval),
	)
}

type observationRecords struct {
	mu      sync.Mutex
	traces  []observe.TraceRecord
	metrics []observe.MetricRecord
	audits  []observe.AuditRecord
	usages  []observe.UsageRecord
	changed chan struct{}
}

func (r *observationRecords) trace(_ context.Context, record observe.TraceRecord) error {
	r.mu.Lock()
	r.traces = append(r.traces, record)
	r.mu.Unlock()
	return nil
}
func (r *observationRecords) metric(_ context.Context, record observe.MetricRecord) error {
	r.mu.Lock()
	r.metrics = append(r.metrics, record)
	r.mu.Unlock()
	return nil
}
func (r *observationRecords) audit(_ context.Context, record observe.AuditRecord) error {
	r.mu.Lock()
	r.audits = append(r.audits, record)
	r.mu.Unlock()
	return nil
}
func (r *observationRecords) usage(_ context.Context, record observe.UsageRecord) error {
	r.mu.Lock()
	r.usages = append(r.usages, record)
	r.mu.Unlock()
	return nil
}
func (r *observationRecords) hasTrace(kind observe.TraceKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.traces {
		if record.Kind == kind {
			return true
		}
	}
	return false
}
func (r *observationRecords) hasMetric(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.metrics {
		if record.Name == name {
			return true
		}
	}
	return false
}
func (r *observationRecords) tracesCopy() []observe.TraceRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observe.TraceRecord(nil), r.traces...)
}
func (r *observationRecords) metricsCopy() []observe.MetricRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observe.MetricRecord(nil), r.metrics...)
}
func (r *observationRecords) auditsCopy() []observe.AuditRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observe.AuditRecord(nil), r.audits...)
}
func (r *observationRecords) usageCopy() []observe.UsageRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observe.UsageRecord(nil), r.usages...)
}
