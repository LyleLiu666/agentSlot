package grpcchannel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type Client struct{ connection grpc.ClientConnInterface }

func NewClient(connection grpc.ClientConnInterface) *Client { return &Client{connection: connection} }

func (c *Client) ListSessions(ctx context.Context, request interaction.ListSessionsRequest) (interaction.SessionList, error) {
	return call[interaction.ListSessionsRequest, interaction.SessionList](c, ctx, OperationListSessions, request)
}
func (c *Client) CreateSession(ctx context.Context, request interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	return call[interaction.CreateSessionRequest, interaction.SessionOpened](c, ctx, OperationCreateSession, request)
}
func (c *Client) ResumeSession(ctx context.Context, request interaction.ResumeSessionRequest) (interaction.SessionOpened, error) {
	return call[interaction.ResumeSessionRequest, interaction.SessionOpened](c, ctx, OperationResumeSession, request)
}
func (c *Client) ForkSession(ctx context.Context, request interaction.ForkSessionRequest) (interaction.SessionOpened, error) {
	return call[interaction.ForkSessionRequest, interaction.SessionOpened](c, ctx, OperationForkSession, request)
}
func (c *Client) StartSessionFromSummary(ctx context.Context, request interaction.SummarySessionRequest) (interaction.SessionOpened, error) {
	return call[interaction.SummarySessionRequest, interaction.SessionOpened](c, ctx, OperationStartSessionFromSummary, request)
}
func (c *Client) Send(ctx context.Context, request interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.SendRequest, interaction.EnqueueReceipt](c, ctx, OperationSend, request)
}
func (c *Client) SendAndWait(ctx context.Context, request interaction.SendRequest) (interaction.RunResult, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.SendRequest, interaction.RunResult](c, ctx, OperationSendAndWait, request)
}
func (c *Client) Steer(ctx context.Context, request interaction.SteerRequest) (interaction.EnqueueReceipt, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.SteerRequest, interaction.EnqueueReceipt](c, ctx, OperationSteer, request)
}
func (c *Client) RunPending(ctx context.Context, request interaction.RunPendingRequest) (interaction.RunReceipt, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.RunPendingRequest, interaction.RunReceipt](c, ctx, OperationRunPending, request)
}
func (c *Client) Cancel(ctx context.Context, request interaction.CancelRequest) error {
	request.Actor = agent.ActorIdentity{}
	_, err := call[interaction.CancelRequest, struct{}](c, ctx, OperationCancel, request)
	return err
}
func (c *Client) WhenIdle(ctx context.Context, request interaction.WhenIdleRequest) error {
	_, err := call[interaction.WhenIdleRequest, struct{}](c, ctx, OperationWhenIdle, request)
	return err
}
func (c *Client) EditQueued(ctx context.Context, request interaction.EditQueuedRequest) (interaction.CommitReceipt, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.EditQueuedRequest, interaction.CommitReceipt](c, ctx, OperationEditQueued, request)
}
func (c *Client) DeleteQueued(ctx context.Context, request interaction.DeleteQueuedRequest) (interaction.CommitReceipt, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.DeleteQueuedRequest, interaction.CommitReceipt](c, ctx, OperationDeleteQueued, request)
}
func (c *Client) ReclassifyQueued(ctx context.Context, request interaction.ReclassifyQueuedRequest) (interaction.CommitReceipt, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.ReclassifyQueuedRequest, interaction.CommitReceipt](c, ctx, OperationReclassifyQueued, request)
}
func (c *Client) ModelConfig(ctx context.Context, request interaction.ModelConfigRequest) (interaction.ModelConfigView, error) {
	return call[interaction.ModelConfigRequest, interaction.ModelConfigView](c, ctx, OperationModelConfig, request)
}
func (c *Client) UpdateModelConfig(ctx context.Context, request interaction.UpdateModelConfigRequest) (interaction.CommitReceipt, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.UpdateModelConfigRequest, interaction.CommitReceipt](c, ctx, OperationUpdateModelConfig, request)
}
func (c *Client) View(ctx context.Context, request interaction.SessionViewRequest) (interaction.SessionView, error) {
	return call[interaction.SessionViewRequest, interaction.SessionView](c, ctx, OperationView, request)
}
func (c *Client) History(ctx context.Context, request interaction.HistoryRequest) (interaction.HistoryPage, error) {
	return call[interaction.HistoryRequest, interaction.HistoryPage](c, ctx, OperationHistory, request)
}
func (c *Client) ExtensionDiagnostics(ctx context.Context, request interaction.ExtensionDiagnosticsRequest) (interaction.ExtensionDiagnosticsPage, error) {
	return call[interaction.ExtensionDiagnosticsRequest, interaction.ExtensionDiagnosticsPage](c, ctx, OperationExtensionDiagnostics, request)
}
func (c *Client) Commands(ctx context.Context, request interaction.CommandScope) ([]interaction.CommandDescriptor, error) {
	return call[interaction.CommandScope, []interaction.CommandDescriptor](c, ctx, OperationCommands, request)
}
func (c *Client) InvokeCommand(ctx context.Context, request interaction.CommandInvocation) (interaction.CommandResult, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.CommandInvocation, interaction.CommandResult](c, ctx, OperationInvokeCommand, request)
}
func (c *Client) CloseSession(ctx context.Context, request interaction.CloseSessionRequest) (interaction.CloseSessionReceipt, error) {
	request.Actor = agent.ActorIdentity{}
	return call[interaction.CloseSessionRequest, interaction.CloseSessionReceipt](c, ctx, OperationCloseSession, request)
}

func (c *Client) Subscribe(ctx context.Context, request interaction.SubscribeRequest) (interaction.EventStream, error) {
	if c == nil || c.connection == nil {
		return nil, errors.New("grpcchannel: client connection is required")
	}
	encoded, err := encodeRequest(OperationSubscribe, request)
	if err != nil {
		return nil, err
	}
	streamContext, cancel := context.WithCancel(ctx)
	stream, err := c.connection.NewStream(streamContext, subscribeDescription, subscribeMethod)
	if err != nil {
		cancel()
		return nil, decodeError(err)
	}
	if err := stream.SendMsg(wrapperspb.Bytes(encoded)); err != nil {
		cancel()
		return nil, decodeError(err)
	}
	if err := stream.CloseSend(); err != nil {
		cancel()
		return nil, decodeError(err)
	}
	return &remoteEventStream{stream: stream, cancel: cancel}, nil
}

func call[Request any, Response any](client *Client, ctx context.Context, operation Operation, request Request) (Response, error) {
	var result Response
	if client == nil || client.connection == nil {
		return result, errors.New("grpcchannel: client connection is required")
	}
	encoded, err := encodeRequest(operation, request)
	if err != nil {
		return result, err
	}
	response := new(wrapperspb.BytesValue)
	if err := client.connection.Invoke(ctx, callMethod, wrapperspb.Bytes(encoded), response); err != nil {
		return result, decodeError(err)
	}
	if err := decodeResponse(response, &result); err != nil {
		return result, err
	}
	return result, nil
}

func encodeRequest(operation Operation, request any) ([]byte, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("grpcchannel: request cannot be encoded")
	}
	encoded, err := json.Marshal(wireRequest{Profile: Profile, Operation: operation, Payload: payload})
	if err != nil {
		return nil, errors.New("grpcchannel: request envelope cannot be encoded")
	}
	return encoded, nil
}

func decodeResponse(value *wrapperspb.BytesValue, destination any) error {
	if value == nil || len(value.Value) == 0 {
		return errors.New("grpcchannel: empty response envelope")
	}
	var response wireResponse
	if err := json.Unmarshal(value.Value, &response); err != nil || response.Profile != Profile || len(response.Payload) == 0 {
		return errors.New("grpcchannel: invalid response envelope")
	}
	if err := json.Unmarshal(response.Payload, destination); err != nil {
		return errors.New("grpcchannel: invalid response payload")
	}
	return nil
}

type remoteEventStream struct {
	stream grpc.ClientStream
	cancel context.CancelFunc
	once   sync.Once
}

func (s *remoteEventStream) Recv(ctx context.Context) (interaction.Event, error) {
	var event interaction.Event
	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()
	response := new(wrapperspb.BytesValue)
	if err := s.stream.RecvMsg(response); err != nil {
		if errors.Is(err, io.EOF) {
			return event, interaction.ErrEventStreamClosed
		}
		return event, decodeError(err)
	}
	if err := decodeResponse(response, &event); err != nil {
		return event, err
	}
	return event, nil
}

func (s *remoteEventStream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

var _ interaction.GatewayAccess = (*Client)(nil)
var _ interaction.EventStream = (*remoteEventStream)(nil)
