// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tracing

import (
	"context"
	"io"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/util/tracing/tracingpb"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/opentracing/opentracing-go/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var gRPCComponentTag = opentracing.Tag{Key: string(ext.Component), Value: "gRPC"}

// metadataReaderWriter satisfies both the opentracing.TextMapReader and
// opentracing.TextMapWriter interfaces.
type metadataReaderWriter struct {
	metadata.MD
}

func (w metadataReaderWriter) Set(key, val string) {
	// The GRPC HPACK implementation rejects any uppercase keys here.
	//
	// As such, since the HTTP_HEADERS format is case-insensitive anyway, we
	// blindly lowercase the key (which is guaranteed to work in the
	// Inject/Extract sense per the OpenTracing spec).
	key = strings.ToLower(key)
	w.MD[key] = append(w.MD[key], val)
}

func (w metadataReaderWriter) ForeachKey(handler func(key, val string) error) error {
	for k, vals := range w.MD {
		for _, v := range vals {
			if err := handler(k, v); err != nil {
				return err
			}
		}
	}

	return nil
}

func extractSpanContext(ctx context.Context, tracer *Tracer) (*SpanContext, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	}
	return tracer.Extract(opentracing.HTTPHeaders, metadataReaderWriter{md})
}

// spanInclusionFuncForServer is used as a SpanInclusionFunc for the server-side
// of RPCs, deciding for which operations the gRPC opentracing interceptor should
// create a span.
func spanInclusionFuncForServer(t *Tracer, parentSpanCtx *SpanContext) bool {
	// Is client tracing?
	return (parentSpanCtx != nil && !parentSpanCtx.IsNoop()) ||
		// Should we trace regardless of the client? This is useful for calls coming
		// through the HTTP->RPC gateway (i.e. the AdminUI), where client is never
		// tracing.
		t.AlwaysTrace()
}

// SetSpanTags sets one or more tags on the given span according to the
// error.
func SetSpanTags(sp *Span, err error, client bool) {
	c := otgrpc.ErrorClass(err)
	code := codes.Unknown
	if s, ok := status.FromError(err); ok {
		code = s.Code()
	}
	sp.SetTag("response_code", code)
	sp.SetTag("response_class", c)
	if err == nil {
		return
	}
	if client || c == otgrpc.ServerError {
		sp.SetTag(string(ext.Error), true)
	}
}

// ServerInterceptor returns a grpc.UnaryServerInterceptor suitable
// for use in a grpc.NewServer call.
//
// For example:
//
//     s := grpcutil.NewServer(
//         ...,  // (existing ServerOptions)
//         grpc.UnaryInterceptor(ServerInterceptor(tracer)))
//
// All gRPC server spans will look for an tracing SpanContext in the gRPC
// metadata; if found, the server span will act as the ChildOf that RPC
// SpanContext.
//
// Root or not, the server Span will be embedded in the context.Context for the
// application-specific gRPC handler(s) to access.
func ServerInterceptor(tracer *Tracer) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp interface{}, err error) {
		spanContext, err := extractSpanContext(ctx, tracer)
		if err != nil {
			return nil, err
		}
		if !spanInclusionFuncForServer(tracer, spanContext) {
			return handler(ctx, req)
		}

		serverSpan := tracer.StartSpan(
			info.FullMethod,
			ext.RPCServerOption(spanContext),
			gRPCComponentTag,
		)
		defer serverSpan.Finish()

		ctx = ContextWithSpan(ctx, serverSpan)
		resp, err = handler(ctx, req)
		if err != nil {
			SetSpanTags(serverSpan, err, false)
			serverSpan.LogFields(log.String("event", "error"), log.String("message", err.Error()))
		}
		return resp, err
	}
}

// StreamServerInterceptor returns a grpc.StreamServerInterceptor suitable
// for use in a grpc.NewServer call. The interceptor instruments streaming RPCs by
// creating a single span to correspond to the lifetime of the RPC's stream.
//
// For example:
//
//     s := grpcutil.NewServer(
//         ...,  // (existing ServerOptions)
//         grpc.StreamInterceptor(StreamServerInterceptor(tracer)))
//
// All gRPC server spans will look for a SpanContext in the gRPC
// metadata; if found, the server span will act as the ChildOf that RPC
// SpanContext.
//
// Root or not, the server Span will be embedded in the context.Context for the
// application-specific gRPC handler(s) to access.
func StreamServerInterceptor(tracer *Tracer) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		spanContext, err := extractSpanContext(ss.Context(), tracer)
		if err != nil {
			return err
		}
		if !spanInclusionFuncForServer(tracer, spanContext) {
			return handler(srv, ss)
		}

		serverSpan := tracer.StartSpan(
			info.FullMethod,
			ext.RPCServerOption(spanContext),
			gRPCComponentTag,
		)
		defer serverSpan.Finish()
		ss = &tracingServerStream{
			ServerStream: ss,
			ctx:          ContextWithSpan(ss.Context(), serverSpan),
		}
		err = handler(srv, ss)
		if err != nil {
			SetSpanTags(serverSpan, err, false)
			serverSpan.LogFields(log.String("event", "error"), log.String("message", err.Error()))
		}
		return err
	}
}

type tracingServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (ss *tracingServerStream) Context() context.Context {
	return ss.ctx
}

// spanInclusionFuncForClient is used as a SpanInclusionFunc for the client-side
// of RPCs, deciding for which operations the gRPC opentracing interceptor should
// create a span.
//
// We use this to circumvent the interceptor's work when tracing is
// disabled. Otherwise, the interceptor causes an increase in the
// number of packets (even with an empty context!).
//
// See #17177.
func spanInclusionFuncForClient(parentSpanCtx *SpanContext) bool {
	return parentSpanCtx != nil && !parentSpanCtx.IsNoop()
}

func injectSpanContext(ctx context.Context, tracer *Tracer, clientSpan *Span) context.Context {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	} else {
		md = md.Copy()
	}
	mdWriter := metadataReaderWriter{md}
	err := tracer.Inject(clientSpan.Context(), opentracing.HTTPHeaders, mdWriter)
	// We have no better place to record an error than the Span itself :-/
	if err != nil {
		clientSpan.LogFields(log.String("event", "Tracer.Inject() failed"), log.Error(err))
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// ClientInterceptor returns a grpc.UnaryClientInterceptor suitable
// for use in a grpc.Dial call.
//
// For example:
//
//     conn, err := grpc.Dial(
//         address,
//         ...,  // (existing DialOptions)
//         grpc.WithUnaryInterceptor(ClientInterceptor(tracer)))
//
// All gRPC client spans will inject the tracing SpanContext into the gRPC
// metadata; they will also look in the context.Context for an active
// in-process parent Span and establish a ChildOf reference if such a parent
// Span could be found.
func ClientInterceptor(tracer *Tracer, init func(*Span)) grpc.UnaryClientInterceptor {
	if init == nil {
		init = func(*Span) {}
	}
	return func(
		ctx context.Context,
		method string,
		req, resp interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		var err error
		var parentCtx *SpanContext
		if parent := SpanFromContext(ctx); parent != nil {
			parentCtx = parent.Context()
		}
		if !spanInclusionFuncForClient(parentCtx) {
			return invoker(ctx, method, req, resp, cc, opts...)
		}
		clientSpan := tracer.StartSpan(
			method,
			opentracing.ChildOf(parentCtx),
			ext.SpanKindRPCClient,
			gRPCComponentTag,
		)
		init(clientSpan)
		defer clientSpan.Finish()
		ctx = injectSpanContext(ctx, tracer, clientSpan)
		err = invoker(ctx, method, req, resp, cc, opts...)
		if err != nil {
			SetSpanTags(clientSpan, err, true)
			clientSpan.LogFields(log.String("event", "error"), log.String("message", err.Error()))
		}
		return err
	}
}

// StreamClientInterceptor returns a grpc.StreamClientInterceptor suitable
// for use in a grpc.Dial call. The interceptor instruments streaming RPCs by creating
// a single span to correspond to the lifetime of the RPC's stream.
//
// For example:
//
//     conn, err := grpc.Dial(
//         address,
//         ...,  // (existing DialOptions)
//         grpc.WithStreamInterceptor(otgrpc.StreamClientInterceptor(tracer)))
//
// All gRPC client spans will inject the tracing SpanContext into the gRPC
// metadata; they will also look in the context.Context for an active
// in-process parent Span and establish a ChildOf reference if such a parent
// Span could be found.
func StreamClientInterceptor(tracer *Tracer, init func(*Span)) grpc.StreamClientInterceptor {
	if init == nil {
		init = func(*Span) {}
	}
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		var err error
		var parentCtx *SpanContext
		if parent := SpanFromContext(ctx); parent != nil {
			parentCtx = parent.Context()
		}
		if !spanInclusionFuncForClient(parentCtx) {
			return streamer(ctx, desc, cc, method, opts...)
		}

		clientSpan := tracer.StartSpan(
			method,
			opentracing.ChildOf(parentCtx),
			ext.SpanKindRPCClient,
			gRPCComponentTag,
		)
		init(clientSpan)
		ctx = injectSpanContext(ctx, tracer, clientSpan)
		cs, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			clientSpan.LogFields(log.String("event", "error"), log.String("message", err.Error()))
			SetSpanTags(clientSpan, err, true)
			clientSpan.Finish()
			return cs, err
		}
		return newTracingClientStream(cs, method, desc, clientSpan), nil
	}
}

func newTracingClientStream(
	cs grpc.ClientStream, method string, desc *grpc.StreamDesc, clientSpan *Span,
) grpc.ClientStream {
	finishChan := make(chan struct{})

	isFinished := new(int32)
	*isFinished = 0
	finishFunc := func(err error) {
		// The current OpenTracing specification forbids finishing a span more than
		// once. Since we have multiple code paths that could concurrently call
		// `finishFunc`, we need to add some sort of synchronization to guard against
		// multiple finishing.
		if !atomic.CompareAndSwapInt32(isFinished, 0, 1) {
			return
		}
		close(finishChan)
		defer clientSpan.Finish()
		if err != nil {
			clientSpan.LogFields(log.String("event", "error"), log.String("message", err.Error()))
			SetSpanTags(clientSpan, err, true)
		}
	}
	go func() {
		select {
		case <-finishChan:
			// The client span is being finished by another code path; hence, no
			// action is necessary.
		case <-cs.Context().Done():
			finishFunc(cs.Context().Err())
		}
	}()
	otcs := &tracingClientStream{
		ClientStream: cs,
		desc:         desc,
		finishFunc:   finishFunc,
	}

	// The `ClientStream` interface allows one to omit calling `Recv` if it's
	// known that the result will be `io.EOF`. See
	// http://stackoverflow.com/q/42915337
	// In such cases, there's nothing that triggers the span to finish. We,
	// therefore, set a finalizer so that the span and the context goroutine will
	// at least be cleaned up when the garbage collector is run.
	runtime.SetFinalizer(otcs, func(otcs *tracingClientStream) {
		otcs.finishFunc(nil)
	})
	return otcs
}

type tracingClientStream struct {
	grpc.ClientStream
	desc       *grpc.StreamDesc
	finishFunc func(error)
}

func (cs *tracingClientStream) Header() (metadata.MD, error) {
	md, err := cs.ClientStream.Header()
	if err != nil {
		cs.finishFunc(err)
	}
	return md, err
}

func (cs *tracingClientStream) SendMsg(m interface{}) error {
	err := cs.ClientStream.SendMsg(m)
	if err != nil {
		cs.finishFunc(err)
	}
	return err
}

func (cs *tracingClientStream) RecvMsg(m interface{}) error {
	err := cs.ClientStream.RecvMsg(m)
	if err == io.EOF {
		cs.finishFunc(nil)
		return err
	} else if err != nil {
		cs.finishFunc(err)
		return err
	}
	if !cs.desc.ServerStreams {
		cs.finishFunc(nil)
	}
	return err
}

func (cs *tracingClientStream) CloseSend() error {
	err := cs.ClientStream.CloseSend()
	if err != nil {
		cs.finishFunc(err)
	}
	return err
}

// Recording represents a group of RecordedSpans, as returned by GetRecording.
// Spans are sorted by StartTime.
type Recording []tracingpb.RecordedSpan
