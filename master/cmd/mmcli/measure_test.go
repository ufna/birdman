package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// локальный UDP-эхо: отвечает эхом с задержкой delay; mangle=true — портит payload.
func startEcho(t *testing.T, delay time.Duration, mangle bool) (host string, port int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, raddr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			out := append([]byte(nil), buf[:n]...)
			if mangle {
				out[0] ^= 0xff
			}
			time.AfterFunc(delay, func() { pc.WriteTo(out, raddr) })
		}
	}()
	addr := pc.LocalAddr().(*net.UDPAddr)
	return "127.0.0.1", addr.Port
}

func TestMedian(t *testing.T) {
	ms := func(vs ...int) []time.Duration {
		out := make([]time.Duration, len(vs))
		for i, v := range vs {
			out[i] = time.Duration(v) * time.Millisecond
		}
		return out
	}
	if got := median(ms(30)); got != 30*time.Millisecond {
		t.Fatalf("single: %v", got)
	}
	if got := median(ms(50, 10, 30)); got != 30*time.Millisecond {
		t.Fatalf("odd: %v", got)
	}
	if got := median(ms(40, 10, 20, 30)); got != 25*time.Millisecond {
		t.Fatalf("even (avg middle two): %v", got)
	}
}

func TestProbeEndpointEcho(t *testing.T) {
	host, port := startEcho(t, 0, false)
	rtts := probeEndpoint(host, port, 3, 500*time.Millisecond)
	if len(rtts) != 3 {
		t.Fatalf("want 3 successful probes, got %d", len(rtts))
	}
	for _, r := range rtts {
		if r <= 0 || r > 300*time.Millisecond {
			t.Fatalf("suspicious rtt %v", r)
		}
	}
}

func TestProbeEndpointTimeoutAndMangle(t *testing.T) {
	// никто не слушает → все пробы мимо
	rtts := probeEndpoint("127.0.0.1", 1, 2, 60*time.Millisecond)
	if len(rtts) != 0 {
		t.Fatalf("dead endpoint: want 0, got %d", len(rtts))
	}
	// эхо с испорченным payload → проба не засчитана
	host, port := startEcho(t, 0, true)
	rtts = probeEndpoint(host, port, 2, 120*time.Millisecond)
	if len(rtts) != 0 {
		t.Fatalf("mangled echo must not count, got %d", len(rtts))
	}
}

func TestMeasureRegionsMinAcrossHostsAndSkipDead(t *testing.T) {
	fastHost, fastPort := startEcho(t, 0, false)
	slowHost, slowPort := startEcho(t, 60*time.Millisecond, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/qos" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"qos": []map[string]any{
			{"region": "nl", "host": fastHost, "udp_port": fastPort},
			{"region": "nl", "host": slowHost, "udp_port": slowPort},
			{"region": "dead", "host": "127.0.0.1", "udp_port": 1},
		}})
	}))
	defer srv.Close()
	got, err := measureRegions(&client{base: srv.URL}, 3, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Region != "nl" {
		t.Fatalf("want only nl (dead skipped), got %+v", got)
	}
	// min по хостам региона: быстрый хост должен победить медленный (~60мс)
	if got[0].RTTms >= 60 {
		t.Fatalf("region rtt must come from fastest host, got %dms", got[0].RTTms)
	}
}

func TestMergeManualOverride(t *testing.T) {
	manual := []region{{Region: "nl", RTTms: 99}}
	measured := []region{{Region: "dev", RTTms: 41}, {Region: "nl", RTTms: 12}}
	got := mergeRegions(manual, measured)
	sort.Slice(got, func(i, j int) bool { return got[i].Region < got[j].Region })
	want := []region{{Region: "dev", RTTms: 41}, {Region: "nl", RTTms: 99}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("merge: got %+v want %+v", got, want)
	}
}
