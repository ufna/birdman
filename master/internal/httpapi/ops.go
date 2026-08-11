package httpapi

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ufna/birdman/master/internal/store"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// CommandSender dispatches a command to a node's agent (implemented by
// agentlink.Hub). Non-blocking: commands to offline nodes queue and replay on
// reconnect. cmdID is the stamped id of the queued command.
type CommandSender interface {
	Send(nodeID string, msg *agentlinkv1.MasterMsg) (cmdID string)
}

// --- node drain / undrain (итерация 4, docs/specs/master.md §6) ---

// handleDrainNode marks a node draining: reconcile stops placing new servers
// on it and reaps its ready buffer, allocated servers play their matches out
// (the DB state is authoritative). The agent also gets a Drain command so it
// rejects new StartServer defensively.
func (s *Server) handleDrainNode(w http.ResponseWriter, r *http.Request) {
	s.setNodeDrain(w, r, true)
}

// handleUndrainNode lifts a node drain: the fleet controller resumes placing
// servers, and the agent is told to accept StartServer again.
func (s *Server) handleUndrainNode(w http.ResponseWriter, r *http.Request) {
	s.setNodeDrain(w, r, false)
}

func (s *Server) setNodeDrain(w http.ResponseWriter, r *http.Request, drain bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "node id must be a uuid")
		return
	}
	var node store.Node
	var err error
	if drain {
		node, err = s.st.DrainNode(r.Context(), id)
	} else {
		node, err = s.st.UndrainNode(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) {
		storeError(w, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Best-effort agent notification; the DB node.state drives reconcile even
	// if the agent is offline (the command replays on reconnect).
	if s.sender != nil {
		if drain {
			s.sender.Send(node.ID, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Drain{
				Drain: &agentlinkv1.Drain{Reason: "admin node drain"}}})
		} else {
			s.sender.Send(node.ID, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Undrain{
				Undrain: &agentlinkv1.Undrain{}}})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": node})
}

// handleRevokeNode retires a node for good: state → `dead` (store.RevokeNode).
// Единственный путь в `dead` — до него состояние доводилось только SQL-ом на
// боксе. 409, если у ноды есть живые серверы: живую ноду выводит drain, а
// ревокация оборвала бы идущий матч. Повтор на уже мёртвой ноде — 200 без
// второго события.
//
// Агенту НИЧЕГО не шлём, в отличие от drain: ревокация — заявление «этого бокса
// больше нет». Если он всё-таки жив и переподключится, Hello не воскресит его
// (heartbeat поднимает только из quarantine|down), а команда в очереди мёртвой
// ноды копилась бы впустую.
func (s *Server) handleRevokeNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "node id must be a uuid")
		return
	}
	node, err := s.st.RevokeNode(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": node})
}

// --- server logs proxy (итерация 4, docs/specs/agent.md §5) ---

// handleServerLogs proxies a dedicated-server log stream from the owning node's
// agent to the HTTP client as a chunked text/plain body. Works for dead
// (reaped/failed) servers too — their log files (and gzip archives) stay on the
// node for the retention period. ?follow=1 tails; ?tail=N starts from the last
// N lines. On client disconnect the running agent tail is cancelled.
func (s *Server) handleServerLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "server id must be a uuid")
		return
	}
	if s.sender == nil || s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "logs proxy not wired")
		return
	}
	nodeID, err := s.st.ServerNodeID(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}

	follow := isTrue(r.URL.Query().Get("follow"))
	tail := 0
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "tail must be a non-negative integer")
			return
		}
		tail = n
	}

	// Send the tail, then subscribe by the stamped cmd_id. The agent needs a
	// full network round-trip before any chunk can arrive, so Register always
	// wins the race against the first LogChunk.
	cmdID := s.sender.Send(nodeID, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Tail{
		Tail: &agentlinkv1.TailLogs{ServerId: id, Follow: follow, TailLines: int32(tail)}}})
	ch, cancel := s.logs.Register(cmdID)
	defer cancel()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush() // commit headers so a follow client sees them immediately
	}

	cancelTail := func() {
		// Tell the agent to stop streaming (frees its goroutine on follow).
		s.sender.Send(nodeID, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Tail{
			Tail: &agentlinkv1.TailLogs{CancelCmdId: cmdID}}})
	}

	for {
		select {
		case <-r.Context().Done():
			cancelTail()
			return
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			if data := chunk.GetData(); len(data) > 0 {
				if _, err := w.Write(data); err != nil {
					cancelTail()
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if chunk.GetEof() {
				return
			}
		}
	}
}

// --- agent self-upgrade (итерация 4, docs/specs/agent.md §7, master.md §6) ---

type agentUpgradeRequest struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
	NodeID  string `json:"node_id"` // optional: one node; empty → the whole fleet
}

// redactURLCredentials strips the query string and any userinfo from a URL
// before it is persisted or logged, keeping scheme+host+path — enough to see
// WHERE a binary came from, nothing that grants access to it.
//
// The agent fetches its own binary over a plain GET and speaks no auth
// (agent/internal/upgrade), so the URL we hand it may be a *presigned* registry
// link whose query string is a bearer-equivalent credential — that is exactly
// what the OCI transport of the dev stand produces. The command must carry it
// verbatim; the `events` row must not: those rows live forever and any
// readonly key reads them through the panel, which is served on a public
// domain. Unparseable input is cut at the first '?' rather than passed through.
func redactURLCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexByte(raw, '?'); i >= 0 {
			return raw[:i]
		}
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String()
}

func (s *Server) handleAgentUpgrade(w http.ResponseWriter, r *http.Request) {
	var req agentUpgradeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.URL == "" || req.Version == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "url and version are required")
		return
	}
	if raw, err := hex.DecodeString(strings.ToLower(req.SHA256)); err != nil || len(raw) != 32 {
		writeError(w, http.StatusBadRequest, "bad_request", "sha256 must be 64 hex chars")
		return
	}
	if s.sender == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "agent link not wired")
		return
	}

	var targets []store.Node
	if req.NodeID != "" {
		if _, err := uuid.Parse(req.NodeID); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "node_id must be a uuid")
			return
		}
		node, err := s.st.GetNode(r.Context(), req.NodeID)
		if err != nil {
			storeError(w, err)
			return
		}
		targets = []store.Node{node}
	} else {
		// Апгрейд агента — инфраструктурная операция, а не проектная: цель
		// «все ноды» остаётся всем флотом (пустой фильтр), сужать её по
		// проекту было бы новым поведением, которого никто не просил.
		nodes, err := s.st.ListNodes(r.Context(), store.NodeFilter{})
		if err != nil {
			storeError(w, err)
			return
		}
		targets = nodes
	}

	upgrading := make([]string, 0, len(targets))
	for _, node := range targets {
		if node.State == "dead" {
			continue
		}
		s.sender.Send(node.ID, &agentlinkv1.MasterMsg{Msg: &agentlinkv1.MasterMsg_Upgrade{
			Upgrade: &agentlinkv1.UpgradeAgent{Url: req.URL, Sha256: req.SHA256, Version: req.Version}}})
		nid := node.ID
		if err := s.st.InsertEvent(r.Context(), store.EventAgentUpgrade, store.EventRef{NodeID: &nid},
			map[string]any{"version": req.Version, "url": redactURLCredentials(req.URL)}); err != nil {
			s.log.Error("agent-upgrade: event write failed", "node_id", node.ID, "err", err)
		}
		s.watchAgentUpgrade(node.ID, req.Version)
		upgrading = append(upgrading, node.ID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"upgrading": upgrading, "version": req.Version})
}

// watchAgentUpgrade emits agent_upgrade_succeeded/agent_upgrade_failed once the
// grace window passes, by comparing the node's re-Hello'd agent_version with
// the requested one (agent.md §7: a healthy upgrade reconnects within 60s;
// agent_upgrade_failed feeds the ops.md §1 alert).
func (s *Server) watchAgentUpgrade(nodeID, version string) {
	go func() {
		time.Sleep(70 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		node, err := s.st.GetNode(ctx, nodeID)
		if err != nil {
			s.log.Error("agent-upgrade watchdog: get node failed", "node_id", nodeID, "err", err)
			return
		}
		nid := nodeID
		kind, payload := store.EventAgentUpgradeSucceeded, map[string]any{"version": version}
		if node.AgentVersion != version {
			kind = store.EventAgentUpgradeFailed
			payload["current"] = node.AgentVersion
		}
		if err := s.st.InsertEvent(ctx, kind, store.EventRef{NodeID: &nid}, payload); err != nil {
			s.log.Error("agent-upgrade watchdog: event write failed", "node_id", nodeID, "err", err)
		}
	}()
}

// --- metrics query proxy (итерация 4, docs/specs/panel.md §1.3) ---

// handleMetricsQuery and handleMetricsQueryRange are thin read-only proxies to
// VictoriaMetrics: the panel reads graphs through master, never touching the
// TSDB directly. The query string (query, time / start, end, step) is
// forwarded verbatim.
func (s *Server) handleMetricsQuery(w http.ResponseWriter, r *http.Request) {
	s.proxyVictoria(w, r, "/api/v1/query")
}

func (s *Server) handleMetricsQueryRange(w http.ResponseWriter, r *http.Request) {
	s.proxyVictoria(w, r, "/api/v1/query_range")
}

func (s *Server) proxyVictoria(w http.ResponseWriter, r *http.Request, path string) {
	if s.vmURL == "" {
		writeError(w, http.StatusServiceUnavailable, "metrics_unconfigured",
			"victoriametrics_url is not set on this master")
		return
	}
	target := strings.TrimRight(s.vmURL, "/") + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// --- logs query proxy (Логи v1, docs/superpowers/specs/2026-07-09-centralized-logs-design.md §3) ---

// handleLogsQuery proxies a LogsQL query to VictoriaLogs (history/search for
// the panel). Mirrors the metrics proxy: 503 logs_unconfigured without a
// configured URL, 502 upstream if VictoriaLogs is unreachable. limit is
// clamped on master (default 1000, max 10000) so the panel can't accidentally
// pull unbounded result sets through the proxy.
func (s *Server) handleLogsQuery(w http.ResponseWriter, r *http.Request) {
	if s.vlURL == "" {
		writeError(w, http.StatusServiceUnavailable, "logs_unconfigured",
			"victorialogs_url is not set on this master")
		return
	}
	q := r.URL.Query()
	limit := 1000
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		limit = min(n, 10000)
	}
	q.Set("limit", strconv.Itoa(limit))
	target := strings.TrimRight(s.vlURL, "/") + "/select/logsql/query?" + q.Encode()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
