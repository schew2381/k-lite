package facade

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	klitev1 "github.com/schew2381/k-lite/internal/gen/klitev1"
)

// agentSpawner starts klite-agent processes for one-click joins. The facade
// runs on the machine that runs klited, so "this machine" in the UI's join
// dialog can be a button instead of a copied command. Machines across the
// internet still get the command, since no facade route can reach them.
type agentSpawner struct {
	bin    string // klite-agent binary, absolute
	dir    string // logs and pidfiles, hack/dev-up.sh's layout
	server string // klited address handed to the agent
}

// EnableLocalJoin arms POST /api/nodes/{name}/join. Spawned agents dial the
// facade's first klited endpoint, which is a loopback address in every
// setup this route makes sense for.
func (s *Server) EnableLocalJoin(bin, dir string) {
	server := "127.0.0.1:7443"
	if len(s.endpoints) > 0 {
		server = s.endpoints[0]
	}
	s.spawn = &agentSpawner{bin: bin, dir: dir, server: server}
}

// machineAddresses lists this machine's non-loopback IPv4 addresses. The
// join dialog needs one when its browser only knows localhost, since a
// machine across the network can't dial the loopback endpoint the facade
// itself uses.
func machineAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out
}

// handleJoin mints a join token and starts an agent for the named node on
// this machine, mirroring hack/dev-up.sh: same flags, same log and pidfile
// layout. The response returns as soon as the process is up. Registration
// progress arrives where it already lives, on the node object in the watch.
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if s.spawn == nil {
		writeError(w, http.StatusNotImplemented, "one-click join is off: start the facade with -agent-bin")
		return
	}
	name := r.PathValue("name")
	if _, err := os.Stat(s.spawn.bin); err != nil {
		writeError(w, http.StatusFailedDependency,
			fmt.Sprintf("no klite-agent binary at %s (build it, or point -agent-bin at one)", s.spawn.bin))
		return
	}
	if pid, running := s.spawn.runningAgent(name); running {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("an agent for %s is already running here (pid %d)", name, pid))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	// An agent for an undeclared node retries its join forever, so refusing
	// up front spares the machine a zombie process.
	nodes, err := s.client.List(ctx, &klitev1.ListRequest{Kind: "Node", Name: name})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	if len(nodes.GetObjects()) == 0 {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("%s is not declared: apply its Node YAML first", name))
		return
	}
	resp, err := s.client.NodeToken(ctx, &klitev1.NodeTokenRequest{})
	if err != nil {
		writeRPCError(w, err)
		return
	}
	pid, logPath, err := s.spawn.start(name, resp.GetToken())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pid": pid, "log": logPath})
}

func (sp *agentSpawner) pidfile(node string) string {
	return filepath.Join(sp.dir, "agent-"+node+".pid")
}

// runningAgent reports whether a pidfile for the node names a live process.
// Signal 0 probes liveness without touching the process.
func (sp *agentSpawner) runningAgent(node string) (int, bool) {
	b, err := os.ReadFile(sp.pidfile(node))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	return pid, proc.Signal(syscall.Signal(0)) == nil
}

// start launches the agent in its own session, so it survives facade
// restarts the same way dev-up's disowned agents do.
func (sp *agentSpawner) start(node, token string) (int, string, error) {
	if err := os.MkdirAll(sp.dir, 0o755); err != nil {
		return 0, "", fmt.Errorf("agent dir: %w", err)
	}
	logPath := filepath.Join(sp.dir, "agent-"+node+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, "", fmt.Errorf("agent log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(sp.bin, "--node", node, "--server", sp.server, "--token", token)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, "", fmt.Errorf("start klite-agent: %w", err)
	}
	pid := cmd.Process.Pid
	// A failed pidfile write only weakens the double-join guard, so the
	// error doesn't take the join down with it.
	_ = os.WriteFile(sp.pidfile(node), []byte(strconv.Itoa(pid)+"\n"), 0o644)
	go func() { _ = cmd.Wait() }()
	return pid, logPath, nil
}
