// measure: платформенный приёмочный замер RTT до QoS-эхо регионов
// (итерация 5.2; спека 2026-07-11-iter5-multinode-design.md §4). Продовое
// измерение делает клиент игры — mmcli лишь доказывает флоу «матчмейкер
// выбирает регион по настоящему пингу» без команды игры.
package main

import (
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"sort"
	"time"
)

const probeGap = 50 * time.Millisecond // пауза между пробами одного эндпоинта

// probeEndpoint шлёт n UDP-проб (16-байтовый nonce) на host:port и возвращает
// RTT только подтверждённых эхо (payload совпал байт-в-байт). Мёртвый или
// перевирающий эндпоинт даёт пустой срез.
func probeEndpoint(host string, port, n int, timeout time.Duration) []time.Duration {
	var out []time.Duration
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(probeGap)
		}
		conn, err := net.DialTimeout("udp", addr, timeout)
		if err != nil {
			continue
		}
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			conn.Close()
			continue
		}
		start := time.Now()
		if _, err := conn.Write(nonce); err != nil {
			conn.Close()
			continue
		}
		conn.SetReadDeadline(start.Add(timeout))
		buf := make([]byte, 64)
		rn, err := conn.Read(buf)
		conn.Close()
		if err != nil || rn != len(nonce) || string(buf[:rn]) != string(nonce) {
			continue
		}
		out = append(out, time.Since(start))
	}
	return out
}

// median — медиана непустого среза (для чётного — среднее двух средних).
func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// measureRegions тянет GET /v1/qos и меряет каждый эндпоинт; RTT региона —
// минимум медиан по его хостам (клиент попадёт на ближайшую ноду). Регион без
// единого подтверждённого эха пропускается с WARN в stderr.
func measureRegions(c *client, probes int, timeout time.Duration) ([]region, error) {
	var qos struct {
		QoS []struct {
			Region  string `json:"region"`
			Host    string `json:"host"`
			UDPPort int32  `json:"udp_port"`
		} `json:"qos"`
	}
	if err := c.do("GET", "/v1/qos", nil, &qos); err != nil {
		return nil, fmt.Errorf("qos: %w", err)
	}
	best := map[string]time.Duration{}
	for _, ep := range qos.QoS {
		rtts := probeEndpoint(ep.Host, int(ep.UDPPort), probes, timeout)
		if len(rtts) == 0 {
			fmt.Fprintf(os.Stderr, "mmcli: measure: %s (%s:%d) — нет эха, пропущен\n", ep.Region, ep.Host, ep.UDPPort)
			continue
		}
		m := median(rtts)
		if cur, ok := best[ep.Region]; !ok || m < cur {
			best[ep.Region] = m
		}
	}
	names := make([]string, 0, len(best))
	for name := range best {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]region, 0, len(best))
	parts := make([]string, 0, len(best))
	for _, name := range names {
		ms := best[name].Round(time.Millisecond) / time.Millisecond
		out = append(out, region{Region: name, RTTms: int32(ms)})
		parts = append(parts, fmt.Sprintf("%s=%dms", name, ms))
	}
	if len(parts) > 0 {
		fmt.Fprintf(os.Stderr, "mmcli: measured: %s\n", joinSpace(parts))
	}
	return out, nil
}

func joinSpace(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

// mergeRegions: ручные --region перекрывают измеренные с тем же именем.
func mergeRegions(manual, measured []region) []region {
	seen := map[string]bool{}
	out := append([]region(nil), manual...)
	for _, r := range manual {
		seen[r.Region] = true
	}
	for _, r := range measured {
		if !seen[r.Region] {
			out = append(out, r)
		}
	}
	return out
}
