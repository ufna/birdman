// Package link implements the agent side of the AgentLink gRPC contract
// (docs/specs/protocol.md §1): the agent dials master (outbound only, TLS),
// keeps one Session stream open, sends Hello with the recovered server map
// first, heartbeats every 2s, executes commands (Ack by cmd_id, idempotent)
// and reconnects with exponential backoff.
package link

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// Handler executes master commands. Implementations must return quickly
// (long work goes to goroutines); the outcome travels via ServerEvent /
// PullReport / heartbeats, the Ack only confirms receipt.
type Handler interface {
	Start(ctx context.Context, cmd *agentlinkv1.StartServer)
	Stop(ctx context.Context, cmd *agentlinkv1.StopServer)
	// Allocate delivers a master-allocated match to the dedik's liba
	// (итерация 2: `allocated{match_id, players_expected}` over UDS).
	Allocate(ctx context.Context, cmd *agentlinkv1.AllocateServer)
	PrePull(ctx context.Context, cmd *agentlinkv1.PrePull)
	// Drain / Undrain flip the node-level drain flag (итерация 4): while
	// draining the agent rejects new StartServer commands.
	Drain(ctx context.Context, cmd *agentlinkv1.Drain)
	Undrain(ctx context.Context, cmd *agentlinkv1.Undrain)
	// DrainServer drains ONE dedik (итерация 3, deploy reap): liba gets the
	// `drain{deadline_s, reason}` frame and exits after the match on its own.
	DrainServer(ctx context.Context, cmd *agentlinkv1.DrainServer)
	// Upgrade self-upgrades the agent binary (итерация 4, agent.md §7).
	Upgrade(ctx context.Context, cmd *agentlinkv1.UpgradeAgent)
	// TailLogs streams a server log back as LogChunk frames (итерация 4).
	TailLogs(ctx context.Context, cmd *agentlinkv1.TailLogs)
	// SetRegistries replaces the in-memory private-registry credential
	// snapshot used for host-matched pull auth (registries v1,
	// docs/superpowers/specs/2026-07-09-registries-design.md §2/§3). Sent as
	// a full snapshot at attach (before pending replay) and on every
	// registry change; the agent never persists it to disk.
	SetRegistries(ctx context.Context, cmd *agentlinkv1.SetRegistries)
}

// Source supplies the current node/server state for Hello and heartbeats.
type Source interface {
	Snapshot() []*agentlinkv1.ServerState
	NodeStats() *agentlinkv1.NodeStats
}

// Config parameterizes the link client.
type Config struct {
	MasterAddr    string
	NodeToken     string // enroll/recovery credential + token-fallback Hello; never log
	Hostname      string
	Region        string
	CapacitySlots int32
	AgentVersion  string

	TLSInsecure bool   // dev/loopback only: skip cert verification, token auth, no enroll
	TLSCAFile   string // bootstrap CA (public); unioned with {TLSCertDir}/ca.pem
	// TLSCertDir holds the enrolled mTLS material (client.key 0600, client.crt,
	// ca.pem). When set (and not TLSInsecure / not DialOptions), the client
	// manages mTLS: enroll → dial → renew (mTLS agentlink v1, design §4).
	TLSCertDir    string
	TLSServerName string // ServerName verified on the master leaf; default birdman-master

	HeartbeatEvery time.Duration // default 2s (agent.md §4)
	BackoffMin     time.Duration // default 1s
	BackoffMax     time.Duration // default 30s
	StableAfter    time.Duration // session age that resets backoff; default 5s

	RenewBefore     time.Duration // renew when NotAfter-now < this; default 14d
	RenewCheckEvery time.Duration // renewal ticker while a session is up; default 24h

	// DialOptions overrides transport options entirely (existing token-path
	// tests: bufconn dialer + insecure credentials). When set, the client does
	// NOT manage TLS or enrollment — the caller owns the transport.
	DialOptions []grpc.DialOption
	// DialContext injects a custom dialer (bufconn) while the client still
	// computes TLS credentials and enrolls — the seam for real-mTLS tests.
	DialContext func(context.Context, string) (net.Conn, error)
}

func (c *Config) applyDefaults() {
	if c.HeartbeatEvery <= 0 {
		c.HeartbeatEvery = 2 * time.Second
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 30 * time.Second
	}
	if c.StableAfter <= 0 {
		c.StableAfter = 5 * time.Second
	}
	if c.RenewBefore <= 0 {
		c.RenewBefore = 14 * 24 * time.Hour // design §4 renewal window
	}
	if c.RenewCheckEvery <= 0 {
		c.RenewCheckEvery = 24 * time.Hour // daily ticker
	}
	if c.TLSServerName == "" {
		c.TLSServerName = "birdman-master"
	}
}

// Client maintains the master link.
type Client struct {
	cfg    Config
	h      Handler
	src    Source
	outbox *Outbox
	logf   func(string, ...any)
	seen   *cmdCache

	mu        sync.Mutex
	connected bool

	// certNotAfter is the loaded client cert's NotAfter (unix seconds), 0 when
	// none; read by maybeRenew and the cert-expiry metric.
	certNotAfter atomic.Int64
}

// New creates a link client. logf may be nil.
func New(cfg Config, h Handler, src Source, outbox *Outbox, logf func(string, ...any)) *Client {
	cfg.applyDefaults()
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Client{cfg: cfg, h: h, src: src, outbox: outbox, logf: logf, seen: newCmdCache(1024)}
}

// Connected reports whether a session is currently established (tests).
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}

// Run dials master and keeps a session alive until ctx is done, reconnecting
// with exponential backoff (BackoffMin → BackoffMax cap). Hello with the
// current server map is re-sent on every (re)connect.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.BackoffMin
	for {
		if ctx.Err() != nil {
			return nil
		}
		// Acquire a connection whose credentials match the current TLS material,
		// enrolling first if there is no usable client cert (design §4).
		tr, err := c.acquireTransport(ctx)
		if err != nil {
			c.setConnected(false)
			if ctx.Err() != nil {
				return nil
			}
			var ok bool
			if backoff, ok = c.backoffAfter(ctx, backoff, err, "connect"); !ok {
				return nil
			}
			continue
		}

		// Run sessions on this connection. gRPC transparently re-handshakes the
		// managed conn across transient stream breaks (incl. master restarts);
		// the conn is torn down and rebuilt only when the TLS material changes
		// (renewal → errRenew).
		rebuild := false
		for !rebuild {
			if ctx.Err() != nil {
				break
			}
			started := time.Now()
			serr := c.session(ctx, tr.client, tr.certSes)
			c.setConnected(false)
			if ctx.Err() != nil {
				break
			}
			if errors.Is(serr, errRenew) {
				backoff = c.cfg.BackoffMin
				rebuild = true
				break
			}
			if time.Since(started) >= c.cfg.StableAfter {
				backoff = c.cfg.BackoffMin // the session was healthy — start over
			}
			var ok bool
			if backoff, ok = c.backoffAfter(ctx, backoff, serr, "session"); !ok {
				break
			}
		}
		tr.conn.Close()
	}
}

// backoffAfter logs the reason, applies the PermissionDenied max-backoff cap
// (a bad token/cert is pointless to retry fast), sleeps for the current
// backoff, and returns the next backoff plus whether to keep going (false = ctx
// cancelled during the wait).
func (c *Client) backoffAfter(ctx context.Context, backoff time.Duration, err error, where string) (time.Duration, bool) {
	if status.Code(err) == codes.PermissionDenied {
		backoff = c.cfg.BackoffMax
		c.logf("[link] master rejected node (PermissionDenied) — check node_token/certificate; retry in %s", backoff)
	} else {
		c.logf("[link] %s ended: %v — reconnect in %s", where, err, backoff)
	}
	select {
	case <-ctx.Done():
		return backoff, false
	case <-time.After(backoff):
	}
	if backoff *= 2; backoff > c.cfg.BackoffMax {
		backoff = c.cfg.BackoffMax
	}
	return backoff, true
}

// session runs one Session stream: Hello → heartbeats + outbox + acks until
// the stream breaks.
func (c *Client) session(ctx context.Context, client agentlinkv1.AgentLinkClient, certSession bool) error {
	// Renewal check on every (re)connect (design §4): a cert inside the window
	// is renewed over this live mTLS conn, then the link reconnects with the
	// fresh material.
	if certSession {
		if swapped, err := c.maybeRenew(ctx, client); err != nil {
			c.logf("[link] certificate renewal check failed: %v — continuing with the current cert", err)
		} else if swapped {
			return errRenew
		}
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := client.Session(sctx)
	if err != nil {
		return err
	}
	// On a cert session the certificate IS the identity — the node_token leaves
	// the hot path of the wire protocol (design §4). A token/fallback session
	// (Unimplemented or dev insecure) still carries it, as today.
	token := ""
	if !certSession {
		token = c.cfg.NodeToken
	}
	hello := &agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Hello{Hello: &agentlinkv1.Hello{
		NodeToken:     token,
		Hostname:      c.cfg.Hostname,
		Region:        c.cfg.Region,
		CapacitySlots: c.cfg.CapacitySlots,
		AgentVersion:  c.cfg.AgentVersion,
		Servers:       c.src.Snapshot(),
	}}}
	if err := stream.Send(hello); err != nil {
		return err
	}
	c.setConnected(true)
	c.logf("[link] connected to %s (mtls=%v, hello: %d servers)",
		c.cfg.MasterAddr, certSession, len(hello.GetHello().GetServers()))

	acks := make(chan string, 64)
	recvErr := make(chan error, 1)
	go func() { recvErr <- c.recvLoop(sctx, stream, acks) }()

	ticker := time.NewTicker(c.cfg.HeartbeatEvery)
	defer ticker.Stop()
	// Daily renewal ticker for long-lived sessions (a match box can stay up for
	// weeks). No-op on token sessions.
	renew := time.NewTicker(c.cfg.RenewCheckEvery)
	defer renew.Stop()
	for {
		select {
		case <-sctx.Done():
			return sctx.Err()
		case err := <-recvErr:
			return err
		case <-renew.C:
			if certSession {
				if swapped, err := c.maybeRenew(ctx, client); err != nil {
					c.logf("[link] scheduled certificate renewal failed: %v — will retry", err)
				} else if swapped {
					return errRenew
				}
			}
		case cmdID := <-acks:
			if err := stream.Send(&agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Ack{
				Ack: &agentlinkv1.Ack{CmdId: cmdID},
			}}); err != nil {
				return err
			}
		case <-ticker.C:
			if err := stream.Send(c.heartbeat()); err != nil {
				return err
			}
		case m := <-c.outbox.logChunks():
			// Log chunks are session-scoped: an undelivered chunk dies with
			// the stream (the master closes its side of the tail anyway).
			if err := stream.Send(m); err != nil {
				return err
			}
		case <-c.outbox.wait():
			// A heartbeat goes out before queued events so the master sees
			// consistent server state (port, state) before reacting to the
			// event (in-stream ordering is guaranteed).
			if err := stream.Send(c.heartbeat()); err != nil {
				return err
			}
			for {
				m := c.outbox.peek()
				if m == nil {
					break
				}
				if err := stream.Send(m); err != nil {
					return err // message stays queued for the next session
				}
				c.outbox.pop(m)
			}
		}
	}
}

func (c *Client) heartbeat() *agentlinkv1.AgentMsg {
	return &agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Heartbeat{Heartbeat: &agentlinkv1.Heartbeat{
		TsUnixMs: time.Now().UnixMilli(),
		Node:     c.src.NodeStats(),
		Servers:  c.src.Snapshot(),
	}}}
}

// recvLoop dispatches master commands. Handling is idempotent by cmd_id
// (at-least-once delivery: master replays unacked commands on reconnect) —
// a repeated cmd_id is re-acked without re-execution.
func (c *Client) recvLoop(ctx context.Context, stream agentlinkv1.AgentLink_SessionClient, acks chan<- string) error {
	for {
		in, err := stream.Recv()
		if err != nil {
			return err
		}
		cmdID := commandID(in)
		if cmdID == "" {
			continue // master Ack or unknown — nothing to do in v0
		}
		if c.seen.add(cmdID) {
			c.dispatch(ctx, in)
		} else {
			c.logf("[link] duplicate command %s re-acked (already handled)", cmdID)
		}
		select {
		case acks <- cmdID:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) dispatch(ctx context.Context, in *agentlinkv1.MasterMsg) {
	switch m := in.GetMsg().(type) {
	case *agentlinkv1.MasterMsg_Start:
		c.h.Start(ctx, m.Start)
	case *agentlinkv1.MasterMsg_Stop:
		c.h.Stop(ctx, m.Stop)
	case *agentlinkv1.MasterMsg_Allocate:
		c.h.Allocate(ctx, m.Allocate)
	case *agentlinkv1.MasterMsg_Prepull:
		c.h.PrePull(ctx, m.Prepull)
	case *agentlinkv1.MasterMsg_Drain:
		c.h.Drain(ctx, m.Drain)
	case *agentlinkv1.MasterMsg_Undrain:
		c.h.Undrain(ctx, m.Undrain)
	case *agentlinkv1.MasterMsg_DrainServer:
		c.h.DrainServer(ctx, m.DrainServer)
	case *agentlinkv1.MasterMsg_Upgrade:
		c.h.Upgrade(ctx, m.Upgrade)
	case *agentlinkv1.MasterMsg_Tail:
		c.h.TailLogs(ctx, m.Tail)
	case *agentlinkv1.MasterMsg_SetRegistries:
		c.h.SetRegistries(ctx, m.SetRegistries)
	}
}

// commandID extracts the cmd_id of whichever command the oneof carries.
func commandID(m *agentlinkv1.MasterMsg) string {
	switch c := m.GetMsg().(type) {
	case *agentlinkv1.MasterMsg_Start:
		return c.Start.GetCmdId()
	case *agentlinkv1.MasterMsg_Stop:
		return c.Stop.GetCmdId()
	case *agentlinkv1.MasterMsg_Allocate:
		return c.Allocate.GetCmdId()
	case *agentlinkv1.MasterMsg_Prepull:
		return c.Prepull.GetCmdId()
	case *agentlinkv1.MasterMsg_Drain:
		return c.Drain.GetCmdId()
	case *agentlinkv1.MasterMsg_Undrain:
		return c.Undrain.GetCmdId()
	case *agentlinkv1.MasterMsg_DrainServer:
		return c.DrainServer.GetCmdId()
	case *agentlinkv1.MasterMsg_Upgrade:
		return c.Upgrade.GetCmdId()
	case *agentlinkv1.MasterMsg_Tail:
		return c.Tail.GetCmdId()
	case *agentlinkv1.MasterMsg_SetRegistries:
		return c.SetRegistries.GetCmdId()
	}
	return ""
}

// cmdCache remembers the last N handled cmd_ids (idempotency window).
type cmdCache struct {
	mu   sync.Mutex
	set  map[string]struct{}
	ring []string
	next int
}

func newCmdCache(n int) *cmdCache {
	return &cmdCache{set: make(map[string]struct{}, n), ring: make([]string, n)}
}

// add records id and reports whether it was new.
func (c *cmdCache) add(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.set[id]; ok {
		return false
	}
	if old := c.ring[c.next]; old != "" {
		delete(c.set, old)
	}
	c.ring[c.next] = id
	c.next = (c.next + 1) % len(c.ring)
	c.set[id] = struct{}{}
	return true
}
