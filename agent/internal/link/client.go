// Package link implements the agent side of the AgentLink gRPC contract
// (docs/specs/protocol.md §1): the agent dials master (outbound only, TLS),
// keeps one Session stream open, sends Hello with the recovered server map
// first, heartbeats every 2s, executes commands (Ack by cmd_id, idempotent)
// and reconnects with exponential backoff.
package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
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
}

// Source supplies the current node/server state for Hello and heartbeats.
type Source interface {
	Snapshot() []*agentlinkv1.ServerState
	NodeStats() *agentlinkv1.NodeStats
}

// Config parameterizes the link client.
type Config struct {
	MasterAddr    string
	NodeToken     string // sent in Hello on every connect (v0 auth); never log
	Hostname      string
	Region        string
	CapacitySlots int32
	AgentVersion  string

	TLSInsecure bool   // dev only: skip cert verification (self-signed master)
	TLSCAFile   string // pin a CA instead (production path)

	HeartbeatEvery time.Duration // default 2s (agent.md §4)
	BackoffMin     time.Duration // default 1s
	BackoffMax     time.Duration // default 30s
	StableAfter    time.Duration // session age that resets backoff; default 5s

	// DialOptions overrides transport options entirely (tests: bufconn
	// dialer + insecure credentials).
	DialOptions []grpc.DialOption
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
	opts, err := c.dialOptions()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(c.cfg.MasterAddr, opts...)
	if err != nil {
		return fmt.Errorf("grpc client for %s: %w", c.cfg.MasterAddr, err)
	}
	defer conn.Close()
	client := agentlinkv1.NewAgentLinkClient(conn)

	backoff := c.cfg.BackoffMin
	for {
		started := time.Now()
		err := c.session(ctx, client)
		c.setConnected(false)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) >= c.cfg.StableAfter {
			backoff = c.cfg.BackoffMin // the session was healthy — start over
		}
		if status.Code(err) == codes.PermissionDenied {
			// Bad node token: retrying fast is pointless and noisy.
			backoff = c.cfg.BackoffMax
			c.logf("[link] master rejected node token (PermissionDenied) — check node_token; retry in %s", backoff)
		} else {
			c.logf("[link] session ended: %v — reconnect in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > c.cfg.BackoffMax {
			backoff = c.cfg.BackoffMax
		}
	}
}

// session runs one Session stream: Hello → heartbeats + outbox + acks until
// the stream breaks.
func (c *Client) session(ctx context.Context, client agentlinkv1.AgentLinkClient) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := client.Session(sctx)
	if err != nil {
		return err
	}
	hello := &agentlinkv1.AgentMsg{Msg: &agentlinkv1.AgentMsg_Hello{Hello: &agentlinkv1.Hello{
		NodeToken:     c.cfg.NodeToken,
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
	c.logf("[link] connected to %s (hello: %d servers)",
		c.cfg.MasterAddr, len(hello.GetHello().GetServers()))

	acks := make(chan string, 64)
	recvErr := make(chan error, 1)
	go func() { recvErr <- c.recvLoop(sctx, stream, acks) }()

	ticker := time.NewTicker(c.cfg.HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-sctx.Done():
			return sctx.Err()
		case err := <-recvErr:
			return err
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
	}
	return ""
}

func (c *Client) dialOptions() ([]grpc.DialOption, error) {
	if len(c.cfg.DialOptions) > 0 {
		return c.cfg.DialOptions, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case c.cfg.TLSInsecure:
		// dev only: master runs on a self-signed autogenerated cert
		// (protocol.md §Auth, уточнено в v0).
		tc.InsecureSkipVerify = true
	case c.cfg.TLSCAFile != "":
		pem, err := os.ReadFile(c.cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read tls_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls_ca_file %s: no certificates found", c.cfg.TLSCAFile)
		}
		tc.RootCAs = pool
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tc))}, nil
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
