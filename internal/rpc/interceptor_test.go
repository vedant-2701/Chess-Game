package rpc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const testSecret = "test-shared-secret"

func TestNewUnaryServerAuthInterceptor_ConstructorRejectsEmptySecret(t *testing.T) {
	_, err := NewUnaryServerAuthInterceptor("")
	if err == nil {
		t.Fatal("expected an error for an empty secret, got nil")
	}
}

func TestUnaryServerAuthInterceptor(t *testing.T) {
	interceptor, err := NewUnaryServerAuthInterceptor(testSecret)
	if err != nil {
		t.Fatalf("NewUnaryServerAuthInterceptor: %v", err)
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/matchmaking.v1.MatchReportService/ReportMatchCreated"}
	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "ok", nil
	}

	tests := []struct {
		name    string
		ctx     func() context.Context
		wantErr codes.Code
	}{
		{
			name: "no metadata on the context at all",
			ctx: func() context.Context {
				return context.Background()
			},
			wantErr: codes.Unauthenticated,
		},
		{
			name: "metadata present, secret key absent",
			ctx: func() context.Context {
				md := metadata.New(map[string]string{"some-other-key": "value"})
				return metadata.NewIncomingContext(context.Background(), md)
			},
			wantErr: codes.Unauthenticated,
		},
		{
			name: "secret key present but empty",
			ctx: func() context.Context {
				md := metadata.New(map[string]string{sharedSecretMetadataKey: ""})
				return metadata.NewIncomingContext(context.Background(), md)
			},
			wantErr: codes.Unauthenticated,
		},
		{
			name: "secret key present, wrong value",
			ctx: func() context.Context {
				md := metadata.New(map[string]string{sharedSecretMetadataKey: "wrong-secret"})
				return metadata.NewIncomingContext(context.Background(), md)
			},
			wantErr: codes.Unauthenticated,
		},
		{
			name: "correct secret",
			ctx: func() context.Context {
				md := metadata.New(map[string]string{sharedSecretMetadataKey: testSecret})
				return metadata.NewIncomingContext(context.Background(), md)
			},
			wantErr: codes.OK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerCalled = false
			_, err := interceptor(tt.ctx(), nil, info, handler)

			if tt.wantErr == codes.OK {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				if !handlerCalled {
					t.Error("expected handler to be called on a valid secret")
				}
				return
			}

			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if handlerCalled {
				t.Error("handler must not be called when auth fails")
			}
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("expected a gRPC status error, got: %v", err)
			}
			if st.Code() != tt.wantErr {
				t.Errorf("code: got %v, want %v", st.Code(), tt.wantErr)
			}
		})
	}
}

func TestNewUnaryClientAuthInterceptor_ConstructorRejectsEmptySecret(t *testing.T) {
	_, err := NewUnaryClientAuthInterceptor("")
	if err == nil {
		t.Fatal("expected an error for an empty secret, got nil")
	}
}

func TestUnaryClientAuthInterceptor_AttachesSecretToOutgoingMetadata(t *testing.T) {
	interceptor, err := NewUnaryClientAuthInterceptor(testSecret)
	if err != nil {
		t.Fatalf("NewUnaryClientAuthInterceptor: %v", err)
	}

	var capturedCtx context.Context
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	err = interceptor(context.Background(), "/matchmaking.v1.MatchReportService/ReportMatchCreated", nil, nil, nil, invoker)
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	md, ok := metadata.FromOutgoingContext(capturedCtx)
	if !ok {
		t.Fatal("expected outgoing metadata to be set on the context passed to invoker")
	}
	values := md.Get(sharedSecretMetadataKey)
	if len(values) != 1 || values[0] != testSecret {
		t.Errorf("metadata[%q]: got %v, want [%q]", sharedSecretMetadataKey, values, testSecret)
	}
}

func TestUnaryClientAuthInterceptor_PropagatesInvokerError(t *testing.T) {
	interceptor, err := NewUnaryClientAuthInterceptor(testSecret)
	if err != nil {
		t.Fatalf("NewUnaryClientAuthInterceptor: %v", err)
	}

	wantErr := errors.New("boom")
	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		return wantErr
	}

	err = interceptor(context.Background(), "/matchmaking.v1.MatchReportService/ReportMatchCreated", nil, nil, nil, invoker)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected the invoker's error to propagate, got: %v", err)
	}
}
