// Package jsonlines provides a simple official observation sink that writes
// one typed JSON object per line to an explicitly supplied writer.
package jsonlines

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sync"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/observe"
)

type Recorder struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

var (
	_ observe.TraceSink     = (*Recorder)(nil)
	_ observe.MetricSink    = (*Recorder)(nil)
	_ observe.AuditSink     = (*Recorder)(nil)
	_ observe.UsageRecorder = (*Recorder)(nil)
)

func New(writer io.Writer) (*Recorder, error) {
	if nilWriter(writer) {
		return nil, errors.New("jsonlines: writer is required")
	}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return &Recorder{encoder: encoder}, nil
}

func NewModule(id string, writer io.Writer) (agentslot.Module, error) {
	if id == "" {
		return nil, errors.New("jsonlines: module ID is required")
	}
	recorder, err := New(writer)
	if err != nil {
		return nil, err
	}
	return module{id: id, recorder: recorder}, nil
}

type module struct {
	id       string
	recorder *Recorder
}

func (m module) ID() string { return m.id }
func (m module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Append(observe.TraceSlot, observe.TraceSink(m.recorder)),
		agentslot.Append(observe.MetricSlot, observe.MetricSink(m.recorder)),
		agentslot.Append(observe.AuditSlot, observe.AuditSink(m.recorder)),
		agentslot.Append(observe.UsageSlot, observe.UsageRecorder(m.recorder)),
	)
}

func (r *Recorder) RecordTrace(ctx context.Context, record observe.TraceRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	return r.write(ctx, "trace", record)
}

func (r *Recorder) RecordMetric(ctx context.Context, record observe.MetricRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	return r.write(ctx, "metric", record)
}

func (r *Recorder) RecordAudit(ctx context.Context, record observe.AuditRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	return r.write(ctx, "audit", record)
}

func (r *Recorder) RecordUsage(ctx context.Context, record observe.UsageRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	return r.write(ctx, "usage", record)
}

func (r *Recorder) write(ctx context.Context, kind string, record any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.encoder.Encode(struct {
		Type   string `json:"type"`
		Record any    `json:"record"`
	}{Type: kind, Record: record})
}

func nilWriter(value io.Writer) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
