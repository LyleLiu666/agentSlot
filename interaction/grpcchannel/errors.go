package grpcchannel

import (
	"context"
	"errors"
	"strconv"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func encodeError(err error) error {
	if err == nil {
		return nil
	}
	kind := agent.KindOf(err)
	code := agent.CodeOf(err)
	message := "remote Gateway operation failed"
	var classified *agent.ClassifiedError
	if errors.As(err, &classified) && classified.Message != "" {
		message = classified.Message
	}
	metadata := map[string]string{"kind": string(kind), "code": string(code)}
	grpcCode := grpcCodeFor(kind)
	if errors.Is(err, interaction.ErrEventStreamOverflow) {
		grpcCode = codes.ResourceExhausted
		metadata["code"] = "event_stream_overflow"
		message = "event stream subscriber fell behind"
	}
	var conflict *interaction.RevisionConflictError
	if errors.As(err, &conflict) {
		grpcCode = codes.Aborted
		metadata["current_revision"] = strconv.FormatUint(uint64(conflict.CurrentRevision), 10)
		metadata["snapshot_required"] = strconv.FormatBool(conflict.SnapshotRequired)
		if code == "" {
			metadata["kind"] = string(agent.ErrorConflict)
			metadata["code"] = string(agent.CodeRevisionConflict)
		}
	}
	if errors.Is(err, context.Canceled) && kind == agent.ErrorInternal {
		grpcCode = codes.Canceled
		metadata["kind"] = string(agent.ErrorCanceled)
	}
	if errors.Is(err, context.DeadlineExceeded) && kind == agent.ErrorInternal {
		grpcCode = codes.DeadlineExceeded
		metadata["kind"] = string(agent.ErrorDeadline)
	}
	withDetails, detailErr := status.New(grpcCode, message).WithDetails(&errdetails.ErrorInfo{Reason: "AGENTSLOT_GATEWAY_ERROR", Domain: "agentslot", Metadata: metadata})
	if detailErr != nil {
		return status.Error(codes.Internal, "remote Gateway operation failed")
	}
	return withDetails.Err()
}

func decodeError(err error) error {
	if err == nil {
		return nil
	}
	parsed, ok := status.FromError(err)
	if !ok {
		return err
	}
	for _, detail := range parsed.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok || info.Reason != "AGENTSLOT_GATEWAY_ERROR" {
			continue
		}
		kind := agent.ErrorKind(info.Metadata["kind"])
		code := agent.ErrorCode(info.Metadata["code"])
		if info.Metadata["code"] == "event_stream_overflow" {
			return interaction.ErrEventStreamOverflow
		}
		cause := agent.NewCodedError(kind, code, "grpcchannel", parsed.Message(), nil)
		if code == agent.CodeRevisionConflict {
			current, _ := strconv.ParseUint(info.Metadata["current_revision"], 10, 64)
			snapshot, _ := strconv.ParseBool(info.Metadata["snapshot_required"])
			return &interaction.RevisionConflictError{CurrentRevision: agent.Revision(current), SnapshotRequired: snapshot, Cause: cause}
		}
		return cause
	}
	if parsed.Code() == codes.Canceled {
		return context.Canceled
	}
	if parsed.Code() == codes.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return err
}

func grpcCodeFor(kind agent.ErrorKind) codes.Code {
	switch kind {
	case agent.ErrorInvalidInput:
		return codes.InvalidArgument
	case agent.ErrorConflict:
		return codes.Aborted
	case agent.ErrorNotFound:
		return codes.NotFound
	case agent.ErrorUnauthorized:
		return codes.Unauthenticated
	case agent.ErrorForbidden:
		return codes.PermissionDenied
	case agent.ErrorCanceled:
		return codes.Canceled
	case agent.ErrorDeadline:
		return codes.DeadlineExceeded
	case agent.ErrorUnavailable:
		return codes.Unavailable
	default:
		return codes.Internal
	}
}
