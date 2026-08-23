package grpcchannel

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	serviceName     = "agentslot.gateway.v1.Gateway"
	callMethod      = "/" + serviceName + "/Call"
	subscribeMethod = "/" + serviceName + "/Subscribe"
)

type gatewayService interface {
	call(context.Context, *wrapperspb.BytesValue) (*wrapperspb.BytesValue, error)
	subscribe(*wrapperspb.BytesValue, grpc.ServerStream) error
}

func registerGatewayService(registrar grpc.ServiceRegistrar, service gatewayService) {
	registrar.RegisterService(&grpc.ServiceDesc{
		ServiceName: serviceName,
		HandlerType: (*gatewayService)(nil),
		Methods:     []grpc.MethodDesc{{MethodName: "Call", Handler: callHandler}},
		Streams:     []grpc.StreamDesc{{StreamName: "Subscribe", Handler: subscribeHandler, ServerStreams: true}},
	}, service)
}

func callHandler(service any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	request := new(wrapperspb.BytesValue)
	if err := decode(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return service.(gatewayService).call(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: service, FullMethod: callMethod}
	return interceptor(ctx, request, info, func(ctx context.Context, request any) (any, error) {
		return service.(gatewayService).call(ctx, request.(*wrapperspb.BytesValue))
	})
}

func subscribeHandler(service any, stream grpc.ServerStream) error {
	request := new(wrapperspb.BytesValue)
	if err := stream.RecvMsg(request); err != nil {
		return err
	}
	return service.(gatewayService).subscribe(request, stream)
}

var subscribeDescription = &grpc.StreamDesc{StreamName: "Subscribe", ServerStreams: true}
