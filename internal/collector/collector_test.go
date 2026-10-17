package collector

import (
	"bytes"
	"context"
	"flag"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ErenOzgu/asterisk-exporter/internal/amisim"
)

var update = flag.Bool("update", false, "rewrite golden files")

var istanbul = time.FixedZone("TRT", 3*3600)

func fixedState() amisim.State {
	start := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	return amisim.State{
		AsteriskVersion: "20.6.0", AMIVersion: "9.0.0", MaxCalls: 100, CoreChannels: 7,
		StartTime: start, ReloadTime: start.Add(2 * time.Hour),
		Channels: []amisim.Channel{
			{Name: "PJSIP/1001-0", State: "Up", Linkedid: "100.1"},
			{Name: "PJSIP/1002-1", State: "Up", Linkedid: "100.1"},
			{Name: "PJSIP/1003-2", State: "Up", Linkedid: "200.2"},
			{Name: "PJSIP/1004-3", State: "Ringing", Linkedid: "200.2"},
		},
		Endpoints: []amisim.Endpoint{
			{Name: "1001", DeviceState: "In use", ActiveChannels: 1},
			{Name: "1002", DeviceState: "Not in use"},
			{Name: "1003", DeviceState: "Unavailable"},
			{Name: "1004", DeviceState: "In Use & Ringing", ActiveChannels: 2},
		},
		Contacts: []amisim.Contact{
			{Endpoint: "1001", Status: "Reachable", RTTUsec: 1500},
			{Endpoint: "1001", Status: "Reachable", RTTUsec: 4000}, // second contact: slowest wins
			{Endpoint: "1002", Status: "Reachable", RTTUsec: 800},
			{Endpoint: "1003", Status: "Unreachable"},
			{Endpoint: "1004", Status: "NonQualified"},
		},
		Rooms: []amisim.Room{{Name: "700", Parties: 3}, {Name: "701", Parties: 1}},
		Peers: []amisim.Peer{
			{Name: "legacy1", Status: "OK (12 ms)", IP: "192.0.2.1"},
			{Name: "legacy2", Status: "LAGGED (350 ms)", IP: "192.0.2.2"},
			{Name: "legacy3", Status: "UNREACHABLE", IP: "192.0.2.3"},
			{Name: "legacy4", Status: "Unmonitored", IP: "192.0.2.4"},
			{Name: "legacy5", Status: "Unmonitored", IP: "-none-"},
		},
	}
}

func serve(t *testing.T, state func() amisim.State, mutate func(*amisim.Server)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &amisim.Server{Username: "exporter", Secret: "s3cret", State: state, Location: istanbul}
	if mutate != nil {
		mutate(srv)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { ln.Close(); srv.CloseConns() })
	return ln.Addr().String()
}

func config(addr string) Config {
	return Config{
		Address: addr, Username: "exporter", Secret: "s3cret", Timeout: 2 * time.Second, Location: istanbul,
		PJSIP: true, SIP: true, ConfBridge: true, PerEndpoint: true, Version: "test",
	}
}

func scrape(t *testing.T, cfg Config) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := New(cfg, nil).Scrape(context.Background()).WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// values parses exposition text into "name{labels}" -> value.
func values(t *testing.T, text string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndex(line, " ")
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("bad sample line %q", line)
		}
		out[line[:i]] = v
	}
	return out
}

func TestScrapeValues(t *testing.T) {
	addr := serve(t, fixedState, nil)
	got := values(t, scrape(t, config(addr)))
	want := map[string]float64{
		"asterisk_up": 1,
		`asterisk_collector_success{collector="core"}`:       1,
		`asterisk_collector_success{collector="channels"}`:   1,
		`asterisk_collector_success{collector="pjsip"}`:      1,
		`asterisk_collector_success{collector="sip"}`:        1,
		`asterisk_collector_success{collector="confbridge"}`: 1,

		"asterisk_core_current_channels":                                     7,
		"asterisk_core_max_calls":                                            100,
		"asterisk_core_start_time_seconds":                                   float64(time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC).Unix()),
		"asterisk_core_last_reload_time_seconds":                             float64(time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC).Unix()),
		`asterisk_info{ami_version="9.0.0",system_name="",version="20.6.0"}`: 1,

		`asterisk_channels{state="up"}`:      3,
		`asterisk_channels{state="ringing"}`: 1,
		`asterisk_channels{state="busy"}`:    0, // known states are always exported
		"asterisk_calls_active":              2,

		`asterisk_pjsip_endpoints{state="in_use"}`:                 1,
		`asterisk_pjsip_endpoints{state="not_in_use"}`:             1,
		`asterisk_pjsip_endpoints{state="unavailable"}`:            1,
		`asterisk_pjsip_endpoints{state="in_use_ringing"}`:         1,
		`asterisk_pjsip_endpoint_up{endpoint="1001"}`:              1,
		`asterisk_pjsip_endpoint_up{endpoint="1003"}`:              0,
		`asterisk_pjsip_endpoint_active_channels{endpoint="1004"}`: 2,
		`asterisk_pjsip_endpoint_active_channels{endpoint="1002"}`: 0, // empty field when idle

		`asterisk_pjsip_contacts{status="reachable"}`:         3,
		`asterisk_pjsip_contacts{status="unreachable"}`:       1,
		`asterisk_pjsip_contacts{status="nonqualified"}`:      1,
		`asterisk_pjsip_contact_rtt_seconds{endpoint="1001"}`: 0.004, // slowest of two contacts
		`asterisk_pjsip_contact_rtt_seconds{endpoint="1002"}`: 0.0008,

		`asterisk_sip_peers{status="ok"}`:          1,
		`asterisk_sip_peers{status="lagged"}`:      1,
		`asterisk_sip_peers{status="unreachable"}`: 1,
		`asterisk_sip_peers{status="unmonitored"}`: 2,
		`asterisk_sip_peer_up{peer="legacy1"}`:     1,
		`asterisk_sip_peer_up{peer="legacy2"}`:     1,
		`asterisk_sip_peer_up{peer="legacy3"}`:     0,
		`asterisk_sip_peer_up{peer="legacy4"}`:     1,
		`asterisk_sip_peer_up{peer="legacy5"}`:     0,

		"asterisk_confbridge_rooms":                     2,
		`asterisk_confbridge_parties{conference="700"}`: 3,
		`asterisk_confbridge_parties{conference="701"}`: 1,
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("missing %s", k)
		} else if g != w {
			t.Errorf("%s = %v, want %v", k, g, w)
		}
	}
	if _, ok := got[`asterisk_pjsip_contact_rtt_seconds{endpoint="1003"}`]; ok {
		t.Error("unreachable contact must not export an RTT")
	}
}

func TestTimezoneMatters(t *testing.T) {
	addr := serve(t, fixedState, nil) // sim prints times in UTC+3
	cfg := config(addr)
	right := values(t, scrape(t, cfg))["asterisk_core_start_time_seconds"]
	cfg.Location = time.UTC // wrong zone: "11:30" read as UTC instead of UTC+3, so exactly 3h late
	wrong := values(t, scrape(t, cfg))["asterisk_core_start_time_seconds"]
	if wrong-right != 3*3600 {
		t.Fatalf("wrong-right = %v, want 10800", wrong-right)
	}
}

func TestEmptySystemIsHealthy(t *testing.T) {
	empty := func() amisim.State {
		return amisim.State{AsteriskVersion: "20", AMIVersion: "9.0.0", StartTime: time.Now(), ReloadTime: time.Now()}
	}
	addr := serve(t, empty, nil)
	got := values(t, scrape(t, config(addr)))
	for _, c := range []string{"core", "channels", "pjsip", "sip", "confbridge"} {
		if got[`asterisk_collector_success{collector="`+c+`"}`] != 1 {
			t.Errorf("collector %s should succeed on an empty PBX", c)
		}
	}
	if got["asterisk_confbridge_rooms"] != 0 || got["asterisk_calls_active"] != 0 || got[`asterisk_pjsip_contacts{status="reachable"}`] != 0 {
		t.Errorf("empty PBX should report zeros: %v", got)
	}
}

func TestOptionalCollectorsAndPerEndpointSwitches(t *testing.T) {
	addr := serve(t, fixedState, nil)
	cfg := config(addr)
	cfg.PJSIP, cfg.SIP, cfg.ConfBridge, cfg.PerEndpoint = false, false, false, false
	out := scrape(t, cfg)
	for _, absent := range []string{"asterisk_pjsip", "asterisk_sip", "asterisk_confbridge"} {
		if strings.Contains(out, absent) {
			t.Errorf("%s* must not be exported when its collector is off", absent)
		}
	}
	cfg.PJSIP, cfg.PerEndpoint = true, false
	out = scrape(t, cfg)
	if !strings.Contains(out, "asterisk_pjsip_endpoints{") || strings.Contains(out, "asterisk_pjsip_endpoint_up") || strings.Contains(out, "contact_rtt") {
		t.Errorf("per-endpoint series must be off, aggregates on:\n%s", out)
	}
}

func TestFailureIsolation(t *testing.T) {
	addr := serve(t, func() amisim.State {
		st := fixedState()
		st.NoSIP = true                                          // module not loaded
		st.Denied = map[string]bool{"ConfbridgeListRooms": true} // AMI user lacks the permission
		return st
	}, nil)
	got := values(t, scrape(t, config(addr)))
	if got["asterisk_up"] != 1 {
		t.Fatal("partial failures must not flip asterisk_up")
	}
	for c, want := range map[string]float64{"core": 1, "channels": 1, "pjsip": 1, "sip": 0, "confbridge": 0} {
		if g := got[`asterisk_collector_success{collector="`+c+`"}`]; g != want {
			t.Errorf("collector %s success = %v, want %v", c, g, want)
		}
	}
	if _, ok := got["asterisk_confbridge_rooms"]; ok {
		t.Error("a failed collector must not leave partial series behind")
	}
	if got[`asterisk_pjsip_endpoints{state="in_use"}`] != 1 {
		t.Error("healthy collectors must still report")
	}
}

func TestPermissionDeniedGetsAHint(t *testing.T) {
	addr := serve(t, func() amisim.State {
		st := fixedState()
		st.Denied = map[string]bool{"ConfbridgeListRooms": true}
		return st
	}, nil)
	var logs bytes.Buffer
	exp := New(config(addr), newTestLogger(&logs))
	exp.Scrape(context.Background())
	exp.Scrape(context.Background()) // same failure again: must not be logged twice
	if n := strings.Count(logs.String(), "scrape failed"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), `write permission \"reporting\"`) {
		t.Errorf("log should name the missing permission:\n%s", logs.String())
	}
}

func TestAsteriskDown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens any more
	cfg := Config{Address: addr, Username: "u", Secret: "s", Timeout: time.Second, PJSIP: true}
	got := values(t, scrape(t, cfg))
	if got["asterisk_up"] != 0 {
		t.Fatal("asterisk_up must be 0")
	}
	for k := range got {
		if strings.Contains(k, "collector_") || strings.HasPrefix(k, "asterisk_channels") {
			t.Errorf("no collector series expected while down, found %s", k)
		}
	}
}

func TestWrongSecret(t *testing.T) {
	addr := serve(t, fixedState, nil)
	cfg := config(addr)
	cfg.Secret = "nope"
	if values(t, scrape(t, cfg))["asterisk_up"] != 0 {
		t.Fatal("asterisk_up must be 0 on authentication failure")
	}
}

func TestScrapeHonoursTimeout(t *testing.T) {
	addr := serve(t, fixedState, func(s *amisim.Server) { s.Stall = "CoreShowChannels" })
	cfg := config(addr)
	cfg.Timeout = 400 * time.Millisecond
	start := time.Now()
	got := values(t, scrape(t, cfg))
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("scrape took %v with a 400ms budget", d)
	}
	if got["asterisk_up"] != 1 || got[`asterisk_collector_success{collector="channels"}`] != 0 {
		t.Errorf("stalled collector should fail, not hang: %v", got)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Not in use": "not_in_use", "In Use & Ringing": "in_use_ringing", "UP": "up",
		"  On Hold ": "on_hold", "": "unknown", "---": "unknown", "Dialing Offhook": "dialing_offhook",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSIPStatus(t *testing.T) {
	for in, want := range map[string]string{
		"OK (12 ms)": "ok", "LAGGED (350 ms)": "lagged", "UNREACHABLE": "unreachable",
		"UNKNOWN": "unknown", "Unmonitored": "unmonitored", "weird": "other", "": "other",
	} {
		if got := sipStatus(in); got != want {
			t.Errorf("sipStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// The full output for a fixed PBX is pinned in testdata/metrics.golden:
// run `go test ./internal/collector -update` after changing metrics on purpose.
func TestGolden(t *testing.T) {
	addr := serve(t, fixedState, nil)
	got := normalizeVolatile(scrape(t, config(addr)))
	const path = "testdata/metrics.golden"
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("metrics output changed; rerun with -update if intended.\n--- got ---\n%s", got)
	}
}

var (
	durationLine = regexp.MustCompile(`(?m)^(asterisk_(?:scrape|collector)_duration_seconds(?:\{[^}]*\})?) .*$`)
	goVersion    = regexp.MustCompile(`goversion="[^"]*"`)
)

func normalizeVolatile(s string) string {
	s = durationLine.ReplaceAllString(s, "$1 0")
	return goVersion.ReplaceAllString(s, `goversion="goX"`)
}
