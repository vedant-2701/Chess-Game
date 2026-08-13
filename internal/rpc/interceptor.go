// Package rpc holds gRPC-layer plumbing shared between chess-server (the
// MatchReportService client, per DECISIONS_LOG_PHASE_3.md ADR-033) and
// matchmaking-service (the MatchReportService server). Both binaries live in
// this same module — matchmaking-service builds into its own container but
// is not a separate Go module — so this package is importable by both
// without duplicating trust-boundary logic across a module boundary.
package rpc

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// sharedSecretMetadataKey is the gRPC metadata key carrying the shared
// secret. Lowercase, no underscores — gRPC requires metadata keys to be
// valid HTTP/2 header names.
const sharedSecretMetadataKey = "x-matchmaking-shared-secret"

// NewUnaryServerAuthInterceptor enforces DECISIONS_LOG_PHASE_3.md ADR-033's
// trust boundary: a shared secret checked via gRPC call metadata, chosen
// specifically because this is Docker Compose, not Kubernetes — Compose has
// no declarative per-service network-policy primitive, so any container on
// the same Compose network reaches any other by default. A deliberate
// interim choice (ADR-033), to be replaced with mTLS post-Phase-3, not
// solved further here.
//
// Returns an error rather than panicking on an empty secret
// (CODING_GUIDELINES.md: no panic in internal/ packages) — an empty secret
// would make every caller trivially authenticate against a missing/empty
// header, silently defeating the entire check, so this is caller
// misconfiguration to surface at startup (main.go, Phase 3 Step 4), not a
// per-request condition.
func NewUnaryServerAuthInterceptor(secret string) (grpc.UnaryServerInterceptor, error) {
	if secret == "" {
		return nil, errors.New("rpc.NewUnaryServerAuthInterceptor: secret must not be empty")
	}

	interceptor := func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			slog.Warn("rpc: rejected unary call, no metadata", "method", info.FullMethod)
			return nil, status.Error(codes.Unauthenticated, "rpc: missing metadata")
		}

		values := md.Get(sharedSecretMetadataKey)
		if len(values) != 1 || values[0] == "" {
			slog.Warn("rpc: rejected unary call, missing shared secret", "method", info.FullMethod)
			return nil, status.Error(codes.Unauthenticated, "rpc: missing shared secret")
		}

		// Constant-time compare: this is a credential check, and a
		// timing-variable comparison leaks how many leading bytes matched.
		if subtle.ConstantTimeCompare([]byte(values[0]), []byte(secret)) != 1 {
			slog.Warn("rpc: rejected unary call, invalid shared secret", "method", info.FullMethod)
			return nil, status.Error(codes.Unauthenticated, "rpc: invalid shared secret")
		}

		return handler(ctx, req)
	}

	return interceptor, nil
}

// NewUnaryClientAuthInterceptor attaches the shared secret to every outgoing
// unary gRPC call's metadata — the client-side half of
// NewUnaryServerAuthInterceptor's check. chess-server's MatchReportService
// client (DECISIONS_LOG_PHASE_3.md ADR-033) dials matchmaking-service with
// this interceptor installed via grpc.WithUnaryInterceptor.
func NewUnaryClientAuthInterceptor(secret string) (grpc.UnaryClientInterceptor, error) {
	if secret == "" {
		return nil, errors.New("rpc.NewUnaryClientAuthInterceptor: secret must not be empty")
	}

	interceptor := func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		ctx = metadata.AppendToOutgoingContext(ctx, sharedSecretMetadataKey, secret)
		return invoker(ctx, method, req, reply, cc, opts...)
	}

	return interceptor, nil
}
