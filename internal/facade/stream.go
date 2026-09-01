package facade

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// ssePing keeps idle proxies from cutting the watch stream.
const ssePing = 15 * time.Second

// handleWatch bridges the Watch RPC to SSE. Each WatchEvent goes out as one
// SSE message whose event field is the short type (ADDED, MODIFIED, DELETED)
// and whose data is the protojson WatchEvent.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "response writer cannot stream")
		return
	}
	var kinds []string
	if q := r.URL.Query().Get("kinds"); q != "" {
		kinds = SplitEndpoints(q)
	}
	ctx := r.Context()
	stream, err := s.client.Watch(ctx, &klitev1.WatchRequest{Kinds: kinds})
	if err != nil {
		writeRPCError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": watch open\n\n")
	fl.Flush()

	events := make(chan *klitev1.WatchEvent)
	errc := make(chan error, 1)
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	ping := time.NewTicker(ssePing)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case err := <-errc:
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled {
				return
			}
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", sseSafe(status.Convert(err).Message()))
			fl.Flush()
			return
		case ev := <-events:
			b, err := protojson.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", shortEventType(ev.GetType()), b)
			fl.Flush()
		}
	}
}

func shortEventType(t klitev1.EventType) string {
	return strings.TrimPrefix(t.String(), "EVENT_TYPE_")
}

// sseSafe strips newlines so an error message cannot break SSE framing.
func sseSafe(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

// handleLogs bridges the Logs RPC to a chunked text/plain response. Only the
// klited holding the target agent's command stream can serve a log stream, so
// this walks the endpoints the way the CLI does (internal/cli/logs.go):
// FailedPrecondition and Unavailable mean "ask the next one".
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "response writer cannot stream")
		return
	}
	q := r.URL.Query()
	tail, _ := strconv.Atoi(q.Get("tail"))
	req := &klitev1.LogsRequest{
		Instance: r.PathValue("name"),
		Follow:   q.Get("follow") == "1" || q.Get("follow") == "true",
		Tail:     int32(tail),
	}

	var lastErr error
	for _, ep := range s.endpoints {
		done, err := s.logsFrom(w, fl, r, ep, req)
		if done {
			if err != nil {
				writeRPCError(w, err)
			}
			return
		}
		lastErr = err
	}
	if lastErr != nil {
		writeRPCError(w, lastErr)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "no endpoint could serve the log stream")
}

// logsFrom streams one endpoint's answer. done=false means this endpoint
// cannot serve the stream and the caller should try the next; the response has
// not been written yet in that case, so an HTTP error is still possible.
func (s *Server) logsFrom(w http.ResponseWriter, fl http.Flusher, r *http.Request, ep string, req *klitev1.LogsRequest) (done bool, err error) {
	conn, client, err := s.dialOne(ep)
	if err != nil {
		return true, err
	}
	defer conn.Close()
	stream, err := client.Logs(r.Context(), req)
	if err != nil {
		return !retryElsewhere(err), err
	}
	wrote := false
	for {
		chunk, err := stream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			if !wrote {
				startLogResponse(w)
			}
			return true, nil
		case status.Code(err) == codes.Canceled && r.Context().Err() != nil:
			return true, nil // client hung up on a follow
		case err != nil:
			// Before any data, the wrong or dead replica just means "walk
			// on". After data flowed the failure is real, and the status
			// line is already written, so append a marker instead.
			if !wrote {
				return !retryElsewhere(err), err
			}
			fmt.Fprintf(w, "\n--- log stream ended: %s ---\n", status.Convert(err).Message())
			fl.Flush()
			return true, nil
		}
		if !wrote {
			startLogResponse(w)
			wrote = true
		}
		if _, err := w.Write(chunk.GetData()); err != nil {
			return true, nil
		}
		fl.Flush()
	}
}

func startLogResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
}

// retryElsewhere reports whether another endpoint might succeed where this one
// refused.
func retryElsewhere(err error) bool {
	code := status.Code(err)
	return code == codes.FailedPrecondition || code == codes.Unavailable
}
