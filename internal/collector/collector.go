// Package collector turns Asterisk Manager Interface (AMI) answers into
// Prometheus metrics.
//
// Each scrape opens one AMI session, runs a handful of read-only actions and
// closes the session again, so there is no connection state to go stale.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ErenOzgu/asterisk-exporter/internal/ami"
	"github.com/ErenOzgu/asterisk-exporter/internal/expfmt"
)

// Config selects what to scrape and how to reach Asterisk.
type Config struct {
	Address  string        // AMI host:port
	Username string        // AMI user
	Secret   string        // AMI secret
	Timeout  time.Duration // budget for one whole scrape (connect + all actions)

	// Location is the time zone Asterisk reports its startup/reload time in.
	// AMI prints local wall-clock time without an offset.
	Location *time.Location

	PJSIP       bool // PJSIP endpoints and contacts
	SIP         bool // legacy chan_sip peers
	ConfBridge  bool // ConfBridge rooms
	PerEndpoint bool // per-endpoint / per-peer series (can be many on large systems)

	Version string // exporter version, for the build_info metric
}

type collectorFunc func(c *ami.Client, cfg Config, out *expfmt.Set) error

type collectorDef struct {
	name string
	// perm is the AMI write-permission class the actions need; it is only used
	// to make "Permission denied" errors actionable.
	perm    string
	enabled func(Config) bool
	run     collectorFunc
}

var collectors = []collectorDef{
	{"core", "system or reporting", func(Config) bool { return true }, collectCore},
	{"channels", "system or reporting", func(Config) bool { return true }, collectChannels},
	{"pjsip", "system", func(c Config) bool { return c.PJSIP }, collectPJSIP},
	{"sip", "system or reporting", func(c Config) bool { return c.SIP }, collectSIP},
	{"confbridge", "reporting", func(c Config) bool { return c.ConfBridge }, collectConfBridge},
}

// Exporter scrapes Asterisk on demand.
type Exporter struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex // one AMI session at a time, however many scrapers ask
	lastFail map[string]string
}

// New returns an Exporter. A nil logger discards logs.
func New(cfg Config, log *slog.Logger) *Exporter {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	return &Exporter{cfg: cfg, log: log, lastFail: map[string]string{}}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Scrape collects one snapshot of metrics. It never fails: when Asterisk is
// unreachable the snapshot says so through asterisk_up 0.
func (e *Exporter) Scrape(ctx context.Context) *expfmt.Set {
	e.mu.Lock()
	defer e.mu.Unlock()

	start := time.Now()
	out := expfmt.New()
	out.Add("asterisk_exporter_build_info", "Build information about the exporter.", expfmt.Gauge, 1,
		"version", e.cfg.Version, "goversion", runtime.Version())

	ctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	up := 0.0
	if c, err := e.connect(ctx); err != nil {
		e.noteFailure("connect", err)
	} else {
		up = 1
		e.noteRecovery("connect")
		for _, def := range collectors {
			if def.enabled(e.cfg) {
				e.runCollector(c, def, out)
			}
		}
		_ = c.Close()
	}
	out.Add("asterisk_up", "Whether the last scrape could log in to the Asterisk Manager Interface (1) or not (0).", expfmt.Gauge, up)
	out.Add("asterisk_scrape_duration_seconds", "Duration of the whole scrape.", expfmt.Gauge, time.Since(start).Seconds())
	return out
}

func (e *Exporter) connect(ctx context.Context) (*ami.Client, error) {
	c, err := ami.Dial(ctx, e.cfg.Address, e.cfg.Timeout)
	if err != nil {
		return nil, err
	}
	if err := c.Login(e.cfg.Username, e.cfg.Secret); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (e *Exporter) runCollector(c *ami.Client, def collectorDef, out *expfmt.Set) {
	begin := time.Now()
	tmp := expfmt.New() // committed only when the collector succeeds, so no half-filled families
	err := def.run(c, e.cfg, tmp)
	success := 1.0
	if err != nil {
		success = 0
		if ami.IsPermissionDenied(err) {
			err = fmt.Errorf("%w (the AMI user needs write permission %q)", err, def.perm)
		}
		e.noteFailure(def.name, err)
	} else {
		out.Merge(tmp)
		e.noteRecovery(def.name)
	}
	out.Add("asterisk_collector_success", "Whether the collector succeeded (1) or failed (0) in the last scrape.", expfmt.Gauge, success, "collector", def.name)
	out.Add("asterisk_collector_duration_seconds", "Duration of the collector in the last scrape.", expfmt.Gauge, time.Since(begin).Seconds(), "collector", def.name)
}

// noteFailure logs a failure once per distinct message instead of on every scrape.
func (e *Exporter) noteFailure(what string, err error) {
	msg := err.Error()
	if e.lastFail[what] != msg {
		e.lastFail[what] = msg
		e.log.Warn("scrape failed", "collector", what, "err", msg)
	}
}

func (e *Exporter) noteRecovery(what string) {
	if _, failed := e.lastFail[what]; failed {
		delete(e.lastFail, what)
		e.log.Info("scrape recovered", "collector", what)
	}
}

// ---- helpers ---------------------------------------------------------------

// list runs an event-list action and returns the events with the given name.
// "No ... found" answers count as an empty list.
func list(c *ami.Client, action, event string, kv ...string) ([]ami.Message, error) {
	res, err := c.Do(action, kv...)
	if err != nil {
		if ami.IsEmptyList(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ami.Message
	for _, ev := range res.Events {
		if strings.EqualFold(ev.Get("Event"), event) {
			out = append(out, ev)
		}
	}
	return out, nil
}

// normalize turns a free-form Asterisk state ("In Use & Ringing") into a
// stable label value ("in_use_ringing").
func normalize(s string) string {
	var sb strings.Builder
	pendingSep := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingSep && sb.Len() > 0 {
				sb.WriteByte('_')
			}
			pendingSep = false
			sb.WriteRune(r)
		} else {
			pendingSep = true
		}
	}
	if sb.Len() == 0 {
		return "unknown"
	}
	return sb.String()
}

// num parses an AMI numeric field. Empty or non-numeric values report ok=false.
func num(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err == nil
}

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
