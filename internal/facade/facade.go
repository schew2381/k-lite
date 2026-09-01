// Package facade is the REST/SSE bridge the web UI talks to. It is a pure
// ClusterService gRPC client: no store access, no business logic beyond
// composing List results into the topology view (ADR 0015).
package facade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
	"github.com/schew2381/k-lite/internal/object"
)

// callTimeout bounds one-shot RPCs. Streams live as long as the HTTP request.
const callTimeout = 15 * time.Second

// maxApplyBytes caps a POST /api/apply body.
const maxApplyBytes = 4 << 20

// Server holds the gRPC client and serving options for the HTTP facade.
type Server struct {
	client    klitev1.ClusterServiceClient
	endpoints []string // every klited address, for the per-endpoint log walk
	uiDir     string   // built SPA assets; empty serves the API only
	dev       bool     // permissive CORS for the Vite dev server

	// dialOne is swappable in tests; the default opens a real connection.
	dialOne func(addr string) (io.Closer, klitev1.ClusterServiceClient, error)

	// spawn arms the one-click join route; nil answers 501.
	spawn *agentSpawner
}

// New builds a facade over an already-dialed ClusterService client. The
// creds re-dial individual endpoints for the log walk and may be nil in
// tests that never stream logs.
func New(client klitev1.ClusterServiceClient, endpoints []string, uiDir string, dev bool, creds *Creds) *Server {
	return &Server{
		client:    client,
		endpoints: endpoints,
		uiDir:     uiDir,
		dev:       dev,
		dialOne:   dialOneWith(creds),
	}
}

// Handler wires the route table frozen by ADR 0015.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/topology", s.handleTopology)
	mux.HandleFunc("GET /api/watch", s.handleWatch)
	mux.HandleFunc("GET /api/policycheck", s.handlePolicyCheck)
	mux.HandleFunc("POST /api/apply", s.handleApply)
	mux.HandleFunc("GET /api/instances/{name}/logs", s.handleLogs)
	mux.HandleFunc("POST /api/workloads/{name}/scale", s.handleScale)
	mux.HandleFunc("POST /api/nodes/{name}/drain", s.handleDrain)
	mux.HandleFunc("POST /api/nodes/{name}/uncordon", s.handleUncordon)
	mux.HandleFunc("POST /api/nodes/{name}/join", s.handleJoin)
	mux.HandleFunc("GET /api/nodetoken", s.handleNodeToken)
	mux.HandleFunc("GET /api/{kind}", s.handleList)
	mux.HandleFunc("DELETE /api/{kind}/{name}", s.handleDelete)
	mux.HandleFunc("/", s.handleStatic)
	if s.dev {
		return corsAllowAll(mux)
	}
	return mux
}

// handleList serves GET /api/{kind} as {"items":[...]} in the CLI's JSON
// object form (apiVersion, kind, metadata).
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	kind, err := object.Canonical(r.PathValue("kind"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	resp, err := s.client.List(ctx, &klitev1.ListRequest{Kind: kind, Name: r.URL.Query().Get("name")})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	items := make([]json.RawMessage, 0, len(resp.GetObjects()))
	for _, o := range resp.GetObjects() {
		b, err := object.EncodeJSON(o)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		items = append(items, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleApply forwards a raw multi-document YAML body and returns per-doc results.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxApplyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeError(w, http.StatusBadRequest, "request body is empty; send YAML")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	resp, err := s.client.Apply(ctx, &klitev1.ApplyRequest{Yaml: body})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	writeProto(w, http.StatusOK, resp)
}

// handleDelete serves DELETE /api/{kind}/{name}.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	kind, err := object.Canonical(r.PathValue("kind"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	resp, err := s.client.Delete(ctx, &klitev1.DeleteRequest{Kind: kind, Name: r.PathValue("name")})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	writeProto(w, http.StatusOK, resp)
}

// handlePolicyCheck answers {"available":false} while PolicyCheck is
// Unimplemented (pre-M6), so the UI can hide the simulator instead of erroring.
func (s *Server) handlePolicyCheck(w http.ResponseWriter, r *http.Request) {
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" || to == "" {
		writeError(w, http.StatusBadRequest, "from and to query parameters are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	resp, err := s.client.PolicyCheck(ctx, &klitev1.PolicyCheckRequest{From: from, To: to})
	if status.Code(err) == codes.Unimplemented {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	if err != nil {
		writeRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available":     true,
		"allowed":       resp.GetAllowed(),
		"matchedPolicy": resp.GetMatchedPolicy(),
		"reason":        resp.GetReason(),
	})
}

// handleScale bridges POST /api/workloads/{name}/scale to the Scale RPC,
// which mutates replicas alone under CAS, so a concurrent template edit
// survives (unlike a read-modify-apply).
func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Replicas int32 `json:"replicas"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("body must be {\"replicas\": n}: %v", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	_, err := s.client.Scale(ctx, &klitev1.ScaleRequest{Workload: r.PathValue("name"), Replicas: body.Replicas})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUncordon bridges POST /api/nodes/{name}/uncordon to the Uncordon RPC.
// Cordoning has no route on purpose: live, a cordon only ever happens as the
// first step of a drain.
func (s *Server) handleUncordon(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	_, err := s.client.Uncordon(ctx, &klitev1.UncordonRequest{Node: r.PathValue("name")})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleNodeToken mints a join token and pairs it with the klited endpoints
// this facade dials, which is everything a new machine needs.
func (s *Server) handleNodeToken(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	resp, err := s.client.NodeToken(ctx, &klitev1.NodeTokenRequest{})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     resp.GetToken(),
		"endpoints": s.endpoints,
	})
}

// handleDrain bridges POST /api/nodes/{name}/drain to the streaming Drain RPC
// as chunked text, one progress line per message. The drain is level-based,
// so a client that hangs up mid-stream cancels nothing.
func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "response writer cannot stream")
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	stream, err := s.client.Drain(r.Context(), &klitev1.DrainRequest{Node: r.PathValue("name"), Force: force})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	startLogResponse(w)
	for {
		p, err := stream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return
		case err != nil:
			fmt.Fprintf(w, "drain stream ended: %s\n", status.Convert(err).Message())
			fl.Flush()
			return
		}
		fmt.Fprintln(w, p.GetMessage())
		fl.Flush()
		if p.GetDone() {
			return
		}
	}
}

// handleStatic serves the built SPA with an index.html fallback, so client-side
// routes deep-link cleanly.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		writeError(w, http.StatusNotFound, "unknown API route")
		return
	}
	if s.uiDir == "" {
		writeError(w, http.StatusNotFound, "no UI assets; start klite-facade with --ui-dir")
		return
	}
	// path.Clean collapses any ../ before the join, so requests cannot
	// escape uiDir.
	rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	fp := filepath.Join(s.uiDir, filepath.FromSlash(rel))
	if st, err := os.Stat(fp); err == nil && !st.IsDir() {
		http.ServeFile(w, r, fp)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.uiDir, "index.html"))
}

// corsAllowAll is dev-only: the Vite dev server runs on another port.
func corsAllowAll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("write response", "err", err)
	}
}

// writeProto renders a proto message with protojson, keeping field names in
// the same camelCase form the object codec emits.
func writeProto(w http.ResponseWriter, code int, m proto.Message) {
	b, err := protojson.Marshal(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeRPCError maps a gRPC status to an HTTP status and surfaces the bare
// message, not the wire framing.
func writeRPCError(w http.ResponseWriter, err error) {
	if s, ok := status.FromError(err); ok {
		writeError(w, httpStatus(s.Code()), s.Message())
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}

func httpStatus(code codes.Code) int {
	switch code {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted, codes.FailedPrecondition:
		return http.StatusConflict
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.Canceled:
		return 499 // client closed request (nginx convention)
	default:
		return http.StatusBadGateway
	}
}

// listKind fetches one kind with the standard timeout, shared by the topology
// composer's five calls.
func (s *Server) listKind(ctx context.Context, kind string) ([]*klitev1.Object, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := s.client.List(ctx, &klitev1.ListRequest{Kind: kind})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", strings.ToLower(kind), err)
	}
	return resp.GetObjects(), nil
}

// rpcCause unwraps to the gRPC status error inside a wrapped chain, if any.
func rpcCause(err error) error {
	var cause error = err
	for cause != nil {
		if _, ok := status.FromError(cause); ok {
			return cause
		}
		cause = errors.Unwrap(cause)
	}
	return err
}
