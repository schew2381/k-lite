package server

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/schew2381/k-lite/internal/ca"
)

// Auth is klited's deny-by-default gate (ADR 0013, research/join-auth.md).
// Identity arrives one of two ways: a verified node client cert on the TLS
// session, or the admin bearer token in metadata. Every method belongs to
// exactly one caller class, and anything unlisted is denied:
//
//   - ClusterService        -> admin (bearer token)
//   - AgentService.Register -> open; the handler trades a cluster token (or
//     an existing node cert) for admission
//   - AgentService (rest)   -> node cert
//   - envoy.service.* (xDS) -> node cert, since Envoy dials with the same
//     node identity the agent joined with
type Auth struct {
	adminToken string
}

// NewAuth gates methods against adminToken. An empty token disables the admin
// class outright rather than admitting empty bearers.
func NewAuth(adminToken string) *Auth {
	return &Auth{adminToken: adminToken}
}

type principalKind int

const (
	principalNone principalKind = iota
	principalAdmin
	principalNode
)

type principal struct {
	kind principalKind
	node string // set when kind == principalNode
}

type principalCtxKey struct{}

const registerMethod = "/klite.v1.AgentService/Register"

// Unary returns the unary interceptor half of the gate.
func (a *Auth) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := a.authorize(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream returns the stream interceptor half of the gate.
func (a *Auth) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := a.authorize(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &authedStream{ServerStream: ss, ctx: ctx})
	}
}

// authedStream carries the principal-bearing context to the handler.
type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context { return s.ctx }

// authorize resolves the caller's principal and checks it against the
// method's required class. The returned context carries the principal for
// handlers that match request fields against it.
func (a *Auth) authorize(ctx context.Context, fullMethod string) (context.Context, error) {
	p := a.principalOf(ctx)
	ctx = context.WithValue(ctx, principalCtxKey{}, p)
	switch {
	case fullMethod == registerMethod:
		return ctx, nil
	case strings.HasPrefix(fullMethod, "/klite.v1.AgentService/"), strings.HasPrefix(fullMethod, "/envoy.service."):
		if p.kind == principalNode {
			return ctx, nil
		}
		return nil, denied(p, "node certificate")
	case strings.HasPrefix(fullMethod, "/klite.v1.ClusterService/"):
		if p.kind == principalAdmin {
			return ctx, nil
		}
		return nil, denied(p, "admin token")
	}
	return nil, status.Errorf(codes.PermissionDenied, "method %s is not open to any caller", fullMethod)
}

func denied(p principal, want string) error {
	if p.kind == principalNone {
		return status.Errorf(codes.Unauthenticated, "authentication required: present the %s", want)
	}
	return status.Errorf(codes.PermissionDenied, "this method requires the %s", want)
}

// principalOf inspects the TLS session first: a verified client cert with the
// klite:node: CN is a node. Otherwise the admin bearer token in metadata is
// checked in constant time.
func (a *Auth) principalOf(ctx context.Context) principal {
	if node, ok := peerNode(ctx); ok {
		return principal{kind: principalNode, node: node}
	}
	if a.adminToken != "" && subtle.ConstantTimeCompare([]byte(bearerToken(ctx)), []byte(a.adminToken)) == 1 {
		return principal{kind: principalAdmin}
	}
	return principal{}
}

// peerNode extracts the node name from the connection's verified client cert.
// VerifiedChains is only populated for certs that chained to the cluster CA,
// so no further validation happens here.
func peerNode(ctx context.Context) (string, bool) {
	pr, ok := peer.FromContext(ctx)
	if !ok || pr.AuthInfo == nil {
		return "", false
	}
	tlsInfo, ok := pr.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return "", false
	}
	return ca.NodeFromCert(tlsInfo.State.VerifiedChains[0][0])
}

func bearerToken(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, v := range md.Get("authorization") {
		if tok, found := strings.CutPrefix(v, "Bearer "); found {
			return tok
		}
	}
	return ""
}

// callerPrincipal reads the principal the interceptor stored. Servers built
// without the interceptor (unit tests) see principalNone.
func callerPrincipal(ctx context.Context) principal {
	if p, ok := ctx.Value(principalCtxKey{}).(principal); ok {
		return p
	}
	return principal{}
}

// requireNodeMatch rejects a node principal speaking for a different node.
// Only Register may be reached without a node principal, so handlers call
// this unconditionally.
func requireNodeMatch(ctx context.Context, node string) error {
	if p := callerPrincipal(ctx); p.kind == principalNode && p.node != node {
		return status.Errorf(codes.PermissionDenied, "certificate is for node %q, not %q", p.node, node)
	}
	return nil
}
