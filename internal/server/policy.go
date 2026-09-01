package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
	"github.com/schew2381/k-lite/internal/policy"
)

// PolicyCheck evaluates one from->to connection against the current
// NetworkPolicies, using the same engine that compiles Envoy RBAC, so the
// answer here is the answer the data plane enforces.
func (s *Cluster) PolicyCheck(ctx context.Context, req *klitev1.PolicyCheckRequest) (*klitev1.PolicyCheckResponse, error) {
	if req.GetFrom() == "" || req.GetTo() == "" {
		return nil, status.Error(codes.InvalidArgument, "from and to are required")
	}
	objs, _, err := s.store.List(ctx, object.KindNetworkPolicy)
	if err != nil {
		return nil, storeStatus(err)
	}
	policies := make([]*klitev1.NetworkPolicy, 0, len(objs))
	for _, o := range objs {
		policies = append(policies, o.GetNetworkPolicy())
	}
	d := policy.Evaluate(policies, req.GetFrom(), req.GetTo())
	return &klitev1.PolicyCheckResponse{
		Allowed:       d.Allowed,
		MatchedPolicy: d.MatchedPolicy,
		Reason:        d.Reason,
	}, nil
}
