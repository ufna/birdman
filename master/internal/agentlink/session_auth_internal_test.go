package agentlink

// Internal coverage for the peer-extraction helpers and the certAuth/loopback
// recording that a later task (SetRegistries gate + sessions{auth} metric)
// consumes (mTLS agentlink v1, design §3). These drive Service.authenticate and
// peerIsLoopback directly with synthesised peer contexts, so they exercise the
// exact decision without a full gRPC handshake (the handshake path is covered
// end-to-end in session_auth_test.go).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"net"
	"os"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/ufna/birdman/master/internal/store"
	"github.com/ufna/birdman/master/internal/testdb"
	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// stringAddr is a non-IP net.Addr (e.g. a bufconn address) — it must classify
// as not-loopback.
type stringAddr string

func (a stringAddr) Network() string { return "bufconn" }
func (a stringAddr) String() string  { return string(a) }

func TestPeerIsLoopback(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr // nil → a peer with no address
		want bool
	}{
		{"ipv4 loopback", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}, true},
		{"ipv6 loopback", &net.TCPAddr{IP: net.IPv6loopback, Port: 1234}, true},
		{"public ipv4", &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 1234}, false},
		{"non-ip addr", stringAddr("bufconn"), false},
		{"nil addr", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: c.addr})
			if got := peerIsLoopback(ctx); got != c.want {
				t.Fatalf("peerIsLoopback(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
	// No peer at all in the context → not loopback.
	if peerIsLoopback(context.Background()) {
		t.Fatal("peerIsLoopback with no peer must be false")
	}
}

func testService(t *testing.T, st *store.Store, mode AuthMode) *Service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewService(st, NewHub(log), nil, nil, mode, log)
}

func helloMsg(token string) *agentlinkv1.Hello {
	return &agentlinkv1.Hello{
		NodeToken: token, Hostname: "node-1", Region: "eu", CapacitySlots: 10, AgentVersion: "0.1.0",
	}
}

// A verified client cert makes authenticate resolve the node from the cert CN
// and record certAuth=true; loopback is derived from the peer address.
func TestAuthenticateRecordsCertAuthAndLoopback(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	svc := testService(t, st, AuthMixed)

	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: f.NodeID}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5555},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{leaf}},
		}},
	})

	node, certAuth, loopback, err := svc.authenticate(ctx, helloMsg(""))
	if err != nil {
		t.Fatalf("authenticate (cert): %v", err)
	}
	if node.ID != f.NodeID {
		t.Fatalf("identity from CN: node.ID = %s, want %s", node.ID, f.NodeID)
	}
	if !certAuth {
		t.Fatal("certAuth must be true for a verified client cert")
	}
	if !loopback {
		t.Fatal("loopback must be true for a 127.0.0.1 peer")
	}
}

// The token path records certAuth=false and derives loopback from the (here
// public) peer address.
func TestAuthenticateRecordsTokenAuth(t *testing.T) {
	st := testdb.New(t)
	f := testdb.Seed(t, st, "eu", 10)
	svc := testService(t, st, AuthMixed)

	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 5555},
	})

	node, certAuth, loopback, err := svc.authenticate(ctx, helloMsg(f.NodeToken))
	if err != nil {
		t.Fatalf("authenticate (token): %v", err)
	}
	if node.ID != f.NodeID {
		t.Fatalf("token identity: node.ID = %s, want %s", node.ID, f.NodeID)
	}
	if certAuth {
		t.Fatal("certAuth must be false on the token path")
	}
	if loopback {
		t.Fatal("loopback must be false for a public peer")
	}
}
