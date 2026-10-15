package boxd

import (
	"context"
	"path"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"github.com/discobox-ai/vm/pkg/machine"
)

// debugOptions report every call to boxd's API to machine.DebugCall: a unary
// call with its request and answer, and a stream (Exec, an upload) with its
// first chunk, which names the machine and the command, once it ends.
func debugOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(debugUnary),
		grpc.WithChainStreamInterceptor(debugStream),
	}
}

func debugUnary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if !machine.Debugging() {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	started := time.Now()
	err := invoker(ctx, method, req, reply, cc, opts...)
	machine.DebugCall(started, err, "boxd %s %s -> %s", path.Base(method), debugText(req), debugText(reply))
	return err
}

func debugStream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	if !machine.Debugging() {
		return streamer(ctx, desc, cc, method, opts...)
	}
	started := time.Now()
	stream, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		machine.DebugCall(started, err, "boxd %s", path.Base(method))
		return nil, err
	}
	return &debugClientStream{ClientStream: stream, method: path.Base(method), started: started}, nil
}

// debugClientStream reports a stream when it ends: on the first error from
// RecvMsg, which is io.EOF for a stream that ended well.
type debugClientStream struct {
	grpc.ClientStream
	method  string
	started time.Time

	mu    sync.Mutex
	first string
	done  bool
}

func (s *debugClientStream) SendMsg(m any) error {
	s.mu.Lock()
	if s.first == "" {
		s.first = debugText(m)
	}
	s.mu.Unlock()
	return s.ClientStream.SendMsg(m)
}

func (s *debugClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err == nil {
		return nil
	}
	s.mu.Lock()
	first, done := s.first, s.done
	s.done = true
	s.mu.Unlock()
	if !done {
		machine.DebugCall(s.started, nil, "boxd %s %s (stream)", s.method, first)
	}
	return err
}

// debugText is a message on one line, cut short: a chunk may carry a file.
func debugText(m any) string {
	msg, ok := m.(proto.Message)
	if !ok {
		return "{}"
	}
	text := prototext.MarshalOptions{}.Format(msg)
	if len(text) > 240 {
		text = text[:240] + "..."
	}
	return "{" + text + "}"
}
