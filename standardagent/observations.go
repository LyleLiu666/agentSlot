package standardagent

import (
	"context"
	"sync"

	"github.com/LyleLiu666/agentSlot/observe"
)

const observationBufferLimit = 4096

// observationHub is application-owned and deliberately one-way. It keeps
// optional sinks outside Runtime locks, serializes each chain, isolates sink
// errors and panics, and bounds memory when a sink is slow.
type observationHub struct {
	mu      sync.Mutex
	queue   chan observationEnvelope
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
	traces  []observe.TraceSink
	metrics []observe.MetricSink
	audits  []observe.AuditSink
	usages  []observe.UsageRecorder
}

type observationEnvelope struct {
	trace  *observe.TraceRecord
	metric *observe.MetricRecord
	audit  *observe.AuditRecord
	usage  *observe.UsageRecord
}

func newObservationHub(traces []observe.TraceSink, metrics []observe.MetricSink, audits []observe.AuditSink, usages []observe.UsageRecorder) *observationHub {
	if len(traces)+len(metrics)+len(audits)+len(usages) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	hub := &observationHub{
		queue: make(chan observationEnvelope, observationBufferLimit), done: make(chan struct{}), ctx: ctx, cancel: cancel,
		traces: append([]observe.TraceSink(nil), traces...), metrics: append([]observe.MetricSink(nil), metrics...),
		audits: append([]observe.AuditSink(nil), audits...), usages: append([]observe.UsageRecorder(nil), usages...),
	}
	go hub.run()
	return hub
}

func (h *observationHub) publishTrace(record observe.TraceRecord) {
	if h == nil || record.Validate() != nil {
		return
	}
	copy := record
	h.publish(observationEnvelope{trace: &copy})
}

func (h *observationHub) publishMetric(record observe.MetricRecord) {
	if h == nil || record.Validate() != nil {
		return
	}
	copy := record
	copy.Attributes = cloneStringMap(record.Attributes)
	h.publish(observationEnvelope{metric: &copy})
}

func (h *observationHub) publishAudit(record observe.AuditRecord) {
	if h == nil || record.Validate() != nil {
		return
	}
	copy := record
	h.publish(observationEnvelope{audit: &copy})
}

func (h *observationHub) publishUsage(record observe.UsageRecord) {
	if h == nil || record.Validate() != nil {
		return
	}
	copy := record
	h.publish(observationEnvelope{usage: &copy})
}

func (h *observationHub) publish(envelope observationEnvelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	select {
	case h.queue <- envelope:
	default:
		// Observation is intentionally best-effort. Product requirements that
		// must reject work belong in Policy/Approval, not a passive sink.
	}
}

func (h *observationHub) stop(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.queue)
	}
	done := h.done
	h.mu.Unlock()
	select {
	case <-done:
		h.cancel()
		return nil
	case <-ctx.Done():
		h.cancel()
		return ctx.Err()
	}
}

func (h *observationHub) run() {
	defer close(h.done)
	for envelope := range h.queue {
		switch {
		case envelope.trace != nil:
			for _, sink := range h.traces {
				callTraceSink(h.ctx, sink, *envelope.trace)
			}
		case envelope.metric != nil:
			for _, sink := range h.metrics {
				callMetricSink(h.ctx, sink, *envelope.metric)
			}
		case envelope.audit != nil:
			for _, sink := range h.audits {
				callAuditSink(h.ctx, sink, *envelope.audit)
			}
		case envelope.usage != nil:
			for _, sink := range h.usages {
				callUsageSink(h.ctx, sink, *envelope.usage)
			}
		}
	}
}

func callTraceSink(ctx context.Context, sink observe.TraceSink, record observe.TraceRecord) {
	defer func() { _ = recover() }()
	_ = sink.RecordTrace(ctx, record)
}

func callMetricSink(ctx context.Context, sink observe.MetricSink, record observe.MetricRecord) {
	defer func() { _ = recover() }()
	_ = sink.RecordMetric(ctx, record)
}

func callAuditSink(ctx context.Context, sink observe.AuditSink, record observe.AuditRecord) {
	defer func() { _ = recover() }()
	_ = sink.RecordAudit(ctx, record)
}

func callUsageSink(ctx context.Context, sink observe.UsageRecorder, record observe.UsageRecord) {
	defer func() { _ = recover() }()
	_ = sink.RecordUsage(ctx, record)
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
