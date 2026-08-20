// Package observe defines passive, provider-neutral observation component
// contracts. Sinks receive facts; they cannot mutate Session or Runtime state.
package observe

import (
	"context"
	"errors"
	"math"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

var (
	TraceSlot  = agentslot.Chain[TraceSink]("trace.sink")
	MetricSlot = agentslot.Chain[MetricSink]("metric.sink")
	AuditSlot  = agentslot.Chain[AuditSink]("audit.sink")
	UsageSlot  = agentslot.Chain[UsageRecorder]("usage.recorder")
)

// Identity correlates a fact without carrying message content, tool arguments,
// credentials, or component configuration.
type Identity struct {
	SessionID  agent.SessionID     `json:"session_id,omitempty"`
	RunID      agent.RunID         `json:"run_id,omitempty"`
	StepID     agent.StepID        `json:"step_id,omitempty"`
	ToolCallID agent.ToolCallID    `json:"tool_call_id,omitempty"`
	AttemptID  agent.AttemptID     `json:"attempt_id,omitempty"`
	Actor      agent.ActorIdentity `json:"actor"`
}

type TraceKind string

const (
	TraceRuntimeOpened       TraceKind = "runtime.opened"
	TraceRuntimeClosed       TraceKind = "runtime.closed"
	TraceRunStarted          TraceKind = "run.started"
	TraceRunCompleted        TraceKind = "run.completed"
	TraceRunCanceled         TraceKind = "run.canceled"
	TraceRunFailed           TraceKind = "run.failed"
	TraceModelAttemptStarted TraceKind = "model.attempt.started"
	TraceModelAttemptReset   TraceKind = "model.attempt.reset"
	TraceModelAttemptDone    TraceKind = "model.attempt.completed"
	TraceModelAttemptFailed  TraceKind = "model.attempt.failed"
	TraceToolStarted         TraceKind = "tool.started"
	TraceToolCompleted       TraceKind = "tool.completed"
)

func (k TraceKind) valid() bool {
	switch k {
	case TraceRuntimeOpened, TraceRuntimeClosed, TraceRunStarted, TraceRunCompleted,
		TraceRunCanceled, TraceRunFailed, TraceModelAttemptStarted, TraceModelAttemptReset,
		TraceModelAttemptDone, TraceModelAttemptFailed, TraceToolStarted, TraceToolCompleted:
		return true
	default:
		return false
	}
}

type TraceRecord struct {
	Kind     TraceKind `json:"kind"`
	At       time.Time `json:"at"`
	Identity Identity  `json:"identity"`
}

func (r TraceRecord) Validate() error {
	if !r.Kind.valid() || r.At.IsZero() || !r.Identity.SessionID.Valid() || !r.Identity.Actor.Valid() {
		return errors.New("observe: invalid trace record")
	}
	switch r.Kind {
	case TraceRuntimeOpened, TraceRuntimeClosed:
		return nil
	case TraceRunStarted, TraceRunCompleted, TraceRunCanceled, TraceRunFailed:
		if !r.Identity.RunID.Valid() {
			return errors.New("observe: run trace requires a RunID")
		}
	case TraceModelAttemptStarted, TraceModelAttemptReset, TraceModelAttemptDone, TraceModelAttemptFailed:
		if !r.Identity.RunID.Valid() || !r.Identity.StepID.Valid() || !r.Identity.AttemptID.Valid() {
			return errors.New("observe: model trace requires Run, Step, and Attempt identity")
		}
	case TraceToolStarted, TraceToolCompleted:
		if !r.Identity.RunID.Valid() || !r.Identity.StepID.Valid() || !r.Identity.ToolCallID.Valid() {
			return errors.New("observe: tool trace requires Run, Step, and ToolCall identity")
		}
	}
	return nil
}

type TraceSink interface {
	RecordTrace(context.Context, TraceRecord) error
}

type TraceFunc func(context.Context, TraceRecord) error

func (f TraceFunc) RecordTrace(ctx context.Context, record TraceRecord) error {
	if f == nil {
		return errors.New("observe: nil trace function")
	}
	return f(ctx, record)
}

type MetricKind string

const (
	MetricCounter           MetricKind = "counter"
	MetricDurationMS        MetricKind = "duration_ms"
	MetricRunTotal                     = "agent.run.total"
	MetricModelAttemptTotal            = "agent.model.attempt.total"
	MetricToolCallTotal                = "agent.tool.call.total"
)

type MetricRecord struct {
	Name       string            `json:"name"`
	Kind       MetricKind        `json:"kind"`
	Value      float64           `json:"value"`
	At         time.Time         `json:"at"`
	Identity   Identity          `json:"identity,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

func (r MetricRecord) Validate() error {
	if r.Name == "" || (r.Kind != MetricCounter && r.Kind != MetricDurationMS) || r.At.IsZero() || math.IsNaN(r.Value) || math.IsInf(r.Value, 0) || r.Value < 0 {
		return errors.New("observe: invalid metric record")
	}
	for key := range r.Attributes {
		if key == "" {
			return errors.New("observe: metric attribute keys must be non-empty")
		}
	}
	switch r.Name {
	case MetricRunTotal:
		if !r.Identity.SessionID.Valid() || !r.Identity.RunID.Valid() || !r.Identity.Actor.Valid() {
			return errors.New("observe: run metric requires Session, Run, and Actor identity")
		}
	case MetricModelAttemptTotal:
		if !r.Identity.SessionID.Valid() || !r.Identity.RunID.Valid() || !r.Identity.StepID.Valid() || !r.Identity.AttemptID.Valid() || !r.Identity.Actor.Valid() {
			return errors.New("observe: model metric requires Session, Run, Step, Attempt, and Actor identity")
		}
	case MetricToolCallTotal:
		if !r.Identity.SessionID.Valid() || !r.Identity.RunID.Valid() || !r.Identity.StepID.Valid() || !r.Identity.ToolCallID.Valid() || !r.Identity.Actor.Valid() {
			return errors.New("observe: tool metric requires Session, Run, Step, ToolCall, and Actor identity")
		}
	}
	return nil
}

type MetricSink interface {
	RecordMetric(context.Context, MetricRecord) error
}

type MetricFunc func(context.Context, MetricRecord) error

func (f MetricFunc) RecordMetric(ctx context.Context, record MetricRecord) error {
	if f == nil {
		return errors.New("observe: nil metric function")
	}
	return f(ctx, record)
}

type AuditKind string

const (
	AuditToolDecision       AuditKind = "tool.decision"
	AuditModelConfigChanged AuditKind = "model.config.changed"
)

type AuditRecord struct {
	Kind     AuditKind `json:"kind"`
	At       time.Time `json:"at"`
	Identity Identity  `json:"identity"`
	Action   string    `json:"action"`
	Decision string    `json:"decision"`
}

func (r AuditRecord) Validate() error {
	if (r.Kind != AuditToolDecision && r.Kind != AuditModelConfigChanged) || r.At.IsZero() || !r.Identity.SessionID.Valid() || !r.Identity.Actor.Valid() || r.Action == "" || r.Decision == "" {
		return errors.New("observe: invalid audit record")
	}
	if r.Kind == AuditToolDecision && (!r.Identity.RunID.Valid() || !r.Identity.StepID.Valid() || !r.Identity.ToolCallID.Valid()) {
		return errors.New("observe: tool audit requires Run, Step, and ToolCall identity")
	}
	return nil
}

type AuditSink interface {
	RecordAudit(context.Context, AuditRecord) error
}

type AuditFunc func(context.Context, AuditRecord) error

func (f AuditFunc) RecordAudit(ctx context.Context, record AuditRecord) error {
	if f == nil {
		return errors.New("observe: nil audit function")
	}
	return f(ctx, record)
}

type UsageKind string

const UsageModel UsageKind = "model"

type UsageRecord struct {
	Kind              UsageKind `json:"kind"`
	At                time.Time `json:"at"`
	Identity          Identity  `json:"identity"`
	ProviderKey       string    `json:"provider_key"`
	ModelID           string    `json:"model_id"`
	InputTokens       int64     `json:"input_tokens"`
	OutputTokens      int64     `json:"output_tokens"`
	CachedInputTokens int64     `json:"cached_input_tokens"`
	CacheWriteTokens  int64     `json:"cache_write_tokens"`
	ReasoningTokens   int64     `json:"reasoning_tokens"`
	TotalTokens       int64     `json:"total_tokens"`
	Estimated         bool      `json:"estimated"`
	EstimateSource    string    `json:"estimate_source,omitempty"`
}

func (r UsageRecord) Validate() error {
	if r.Kind != UsageModel || r.At.IsZero() || !r.Identity.SessionID.Valid() || !r.Identity.RunID.Valid() || !r.Identity.StepID.Valid() ||
		!r.Identity.Actor.Valid() || r.ModelID == "" || !r.Identity.AttemptID.Valid() ||
		r.InputTokens < 0 || r.OutputTokens < 0 || r.CachedInputTokens < 0 || r.CacheWriteTokens < 0 || r.ReasoningTokens < 0 || r.TotalTokens < 0 ||
		r.CachedInputTokens > r.InputTokens || r.ReasoningTokens > r.OutputTokens || r.TotalTokens < r.InputTokens+r.OutputTokens ||
		r.Estimated != (r.EstimateSource != "") {
		return errors.New("observe: invalid usage record")
	}
	return nil
}

type UsageRecorder interface {
	RecordUsage(context.Context, UsageRecord) error
}

type UsageFunc func(context.Context, UsageRecord) error

func (f UsageFunc) RecordUsage(ctx context.Context, record UsageRecord) error {
	if f == nil {
		return errors.New("observe: nil usage function")
	}
	return f(ctx, record)
}
