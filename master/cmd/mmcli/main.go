// mmcli is a tiny matchmaking client for birdman-master — the acceptance
// tool of iteration 2 (docs/05-runtime-iterations.md): it files a ticket,
// long-polls it and prints the resulting host:port.
//
//	mmcli --master http://HOST:8100 --key KEY request \
//	      --player p1 --version 1.0.0 --region eu:5 [--region us:80] \
//	      [--measure [--probes 5] [--probe-timeout 700ms]] \
//	      [--project game] [--timeout 120s]
//
// Exit codes: 0 matched, 1 any other terminal status or error, 2 bad usage.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const pollWait = 25 * time.Second // long-poll chunk (protocol.md §3)

type regionFlags []region

type region struct {
	Region string `json:"region"`
	RTTms  int32  `json:"rtt_ms"`
}

func (r *regionFlags) String() string {
	parts := make([]string, 0, len(*r))
	for _, x := range *r {
		parts = append(parts, fmt.Sprintf("%s:%d", x.Region, x.RTTms))
	}
	return strings.Join(parts, ",")
}

func (r *regionFlags) Set(v string) error {
	name, rttRaw, ok := strings.Cut(v, ":")
	if !ok || name == "" {
		return fmt.Errorf("want REGION:RTT_MS, got %q", v)
	}
	rtt, err := strconv.Atoi(rttRaw)
	if err != nil || rtt < 0 {
		return fmt.Errorf("bad rtt in %q", v)
	}
	*r = append(*r, region{Region: name, RTTms: int32(rtt)})
	return nil
}

func main() {
	os.Exit(run())
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintln(os.Stderr, "usage: mmcli --master URL --key KEY request --player P --version 1.0.0 --region NAME:RTT [--region ...] [--measure [--probes 5] [--probe-timeout 700ms]] [--project SLUG] [--timeout 120s]")
	if fs != nil {
		fs.PrintDefaults()
	}
}

func run() int {
	master := flag.String("master", "http://127.0.0.1:8100", "master REST base URL")
	key := flag.String("key", "", "API key (scope matchmaking)")
	flag.Parse()

	if flag.NArg() < 1 {
		usage(nil)
		return 2
	}
	switch flag.Arg(0) {
	case "request":
		return runRequest(*master, *key, flag.Args()[1:])
	default:
		fmt.Fprintf(os.Stderr, "mmcli: unknown command %q\n", flag.Arg(0))
		usage(nil)
		return 2
	}
}

func runRequest(master, key string, args []string) int {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	player := fs.String("player", "", "player id (required)")
	version := fs.String("version", "", "client version, semver (required)")
	project := fs.String("project", "", "project slug (optional on a single-project master)")
	timeout := fs.Duration("timeout", 120*time.Second, "overall wait for a match")
	var regions regionFlags
	fs.Var(&regions, "region", "region with measured rtt as NAME:RTT_MS (repeatable)")
	measure := fs.Bool("measure", false, "measure region rtt via GET /v1/qos UDP echo probes")
	probes := fs.Int("probes", 5, "probes per endpoint with --measure")
	probeTimeout := fs.Duration("probe-timeout", 700*time.Millisecond, "per-probe echo timeout with --measure")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *player == "" || *version == "" || (len(regions) == 0 && !*measure) {
		usage(fs)
		return 2
	}

	c := &client{base: strings.TrimRight(master, "/"), key: key}

	if *measure {
		measured, err := measureRegions(c, *probes, *probeTimeout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mmcli: measure:", err)
			return 1
		}
		regions = regionFlags(mergeRegions(regions, measured))
		if len(regions) == 0 {
			fmt.Fprintln(os.Stderr, "mmcli: measure: ни один регион не ответил на пробы")
			return 1
		}
	}

	// 1. Create the ticket.
	body := map[string]any{
		"player_id":      *player,
		"client_version": *version,
		"regions":        []region(regions),
	}
	if *project != "" {
		body["project"] = *project
	}
	var ticket struct {
		TicketID string `json:"ticket_id"`
		Status   string `json:"status"`
	}
	if err := c.do("POST", "/v1/matchmaking/tickets", body, &ticket); err != nil {
		fmt.Fprintln(os.Stderr, "mmcli: create ticket:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "mmcli: ticket %s %s\n", ticket.TicketID, ticket.Status)

	// 2. Long-poll until terminal or timeout.
	deadline := time.Now().Add(*timeout)
	final := map[string]any{}
	status := ticket.Status
	for status == "queued" && time.Now().Before(deadline) {
		wait := min(pollWait, time.Until(deadline))
		if wait <= 0 {
			break
		}
		final = map[string]any{}
		err := c.do("GET",
			fmt.Sprintf("/v1/matchmaking/tickets/%s?wait=%s", ticket.TicketID, wait.Round(time.Second)),
			nil, &final)
		if err != nil {
			var rl *httpError
			if errors.As(err, &rl) && rl.status == http.StatusTooManyRequests {
				time.Sleep(300 * time.Millisecond) // rate limited — back off
				continue
			}
			fmt.Fprintln(os.Stderr, "mmcli: poll:", err)
			return 1
		}
		status, _ = final["status"].(string)
	}

	if len(final) == 0 { // terminal straight from POST (e.g. update_required)
		if err := c.do("GET", "/v1/matchmaking/tickets/"+ticket.TicketID, nil, &final); err != nil {
			fmt.Fprintln(os.Stderr, "mmcli: get ticket:", err)
			return 1
		}
		status, _ = final["status"].(string)
	}

	// 3. Print the JSON result and, when matched, host:port last.
	out, _ := json.MarshalIndent(final, "", "  ")
	fmt.Println(string(out))
	if status != "matched" {
		fmt.Fprintf(os.Stderr, "mmcli: no match: status=%s\n", status)
		return 1
	}
	match, _ := final["match"].(map[string]any)
	host, _ := match["host"].(string)
	port, _ := match["port"].(float64)
	fmt.Printf("%s:%d\n", host, int(port))
	return 0
}

type client struct {
	base string
	key  string
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, strings.TrimSpace(e.body))
}

func (c *client) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := &http.Client{Timeout: pollWait + 15*time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &httpError{status: resp.StatusCode, body: string(raw)}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("bad response json: %w", err)
		}
	}
	return nil
}
