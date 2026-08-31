package server

import (
	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// Agent registers AgentService so the endpoint exists from day one, and every
// call answers Unimplemented until M2 gives nodes something to do.
type Agent struct {
	klitev1.UnimplementedAgentServiceServer
}

func NewAgent() *Agent {
	return &Agent{}
}
