package model

import (
	"errors"

	agent "github.com/LyleLiu666/agentSlot/agent"
)

// StreamState validates the portable event sequence of one logical model
// call. It contains no Provider wire knowledge and can be reused by Runtime,
// model-backed evaluators, and Executor conformance tests.
type StreamState struct {
	attemptID string
	visible   bool
	terminal  bool
}

// Accept validates and advances one event. Temporary output from one failed
// physical attempt must be reset before another attempt or a failed logical
// terminal can be exposed.
func (s *StreamState) Accept(event ModelEvent) error {
	if s == nil {
		return invalidStream("stream state is unavailable", nil)
	}
	if s.terminal {
		return invalidStream("event arrived after the logical terminal", nil)
	}
	if err := event.Validate(); err != nil {
		return invalidStream("event violates the portable model contract", err)
	}
	requireAttempt := event.Kind == EventDelta || event.Kind == EventReset || event.Kind == EventComplete
	if requireAttempt && !agent.AttemptID(event.AttemptID).Valid() {
		return invalidStream("temporary or completed event requires a valid AttemptID", nil)
	}
	if event.AttemptID != "" && !agent.AttemptID(event.AttemptID).Valid() {
		return invalidStream("event contains an invalid AttemptID", nil)
	}
	switch event.Kind {
	case EventDelta:
		if s.attemptID != "" && s.attemptID != event.AttemptID {
			return invalidStream("AttemptID changed without resetting temporary output", nil)
		}
		s.attemptID = event.AttemptID
		s.visible = true
	case EventReset:
		if !s.visible || s.attemptID == "" || s.attemptID != event.AttemptID {
			return invalidStream("reset does not match visible temporary output", nil)
		}
		s.attemptID = ""
		s.visible = false
	case EventComplete:
		if s.attemptID != "" && s.attemptID != event.AttemptID {
			return invalidStream("completed AttemptID differs from visible output", nil)
		}
		s.terminal = true
	case EventFailed:
		if s.visible {
			return invalidStream("failed stream exposed temporary output without reset", nil)
		}
		if s.attemptID != "" && event.AttemptID != "" && s.attemptID != event.AttemptID {
			return invalidStream("failed AttemptID differs from the active attempt", nil)
		}
		s.terminal = true
	}
	return nil
}

// End validates a Recv terminal error. A normal ErrStreamClosed is only legal
// after one complete or failed event; context and transport errors remain the
// caller's classified failure when the stream has not reached a terminal.
func (s *StreamState) End(err error) error {
	if s == nil {
		return invalidStream("stream state is unavailable", err)
	}
	if errors.Is(err, ErrStreamClosed) {
		if !s.terminal {
			return invalidStream("stream closed before a logical terminal", err)
		}
		return nil
	}
	if s.terminal {
		return invalidStream("stream returned a non-closure error after its logical terminal", err)
	}
	return err
}

// Terminal reports whether a complete or failed event has been accepted.
func (s *StreamState) Terminal() bool { return s != nil && s.terminal }

func invalidStream(message string, cause error) error {
	return agent.NewCodedError(agent.ErrorInternal, agent.CodeModelStreamInvalid, "model.stream", message, cause)
}
