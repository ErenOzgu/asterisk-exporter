// Command asterisk-exporter exposes Asterisk metrics for Prometheus by
// querying the Asterisk Manager Interface (AMI) on every scrape.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // IANA zone names work in minimal containers without /usr/share/zoneinfo

	"github.com/ErenOzgu/asterisk-exporter/internal/collector"
	"github.com/ErenOzgu/asterisk-exporter/internal/expfmt"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	var (
		listen      = flag.String("web.listen-address", ":9650", "Address to serve metrics on.")
		metricsPath = flag.String("web.telemetry-path", "/metrics", "Path to serve metrics on.")
		amiAddr     = flag.String("ami.address", "127.0.0.1:5038", "Asterisk Manager Interface host:port.")
		amiUser     = flag.String("ami.username", os.Getenv("ASTERISK_AMI_USERNAME"), "AMI username (or env ASTERISK_AMI_USERNAME).")
		secretFile  = flag.String("ami.secret-file", "", "File containing the AMI secret. Alternatively set env ASTERISK_AMI_SECRET.")
		timeout     = flag.Duration("ami.timeout", 5*time.Second, "Time budget for one scrape. Keep it below Prometheus' scrape_timeout.")
		tz          = flag.String("asterisk.timezone", "Local", `IANA time zone Asterisk reports its start time in, e.g. "Europe/Istanbul" or "UTC".`)
		pjsip       = flag.Bool("collect.pjsip", true, "Collect PJSIP endpoints and contacts.")
		sip         = flag.Bool("collect.sip", false, "Collect legacy chan_sip peers.")
		confbridge  = flag.Bool("collect.confbridge", true, "Collect ConfBridge rooms.")
		perEndpoint = flag.Bool("collect.per-endpoint", true, "Export per-endpoint / per-peer series. Turn off on very large systems.")
		showVersion = flag.Bool("version", false, "Print the version and exit.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("asterisk-exporter", version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := validateMetricsPath(*metricsPath); err != nil {
		log.Error("bad -web.telemetry-path", "err", err)
		os.Exit(2)
	}
	secret, err := readSecret(*secretFile)
	if err != nil {
		log.Error("cannot read AMI secret", "err", err)
		os.Exit(2)
	}
	if *amiUser == "" || secret == "" {
		log.Error("AMI credentials missing: set -ami.username (or ASTERISK_AMI_USERNAME) and -ami.secret-file (or ASTERISK_AMI_SECRET)")
		os.Exit(2)
	}
	loc := time.Local
	if *tz != "Local" {
		if loc, err = time.LoadLocation(*tz); err != nil {
			log.Error("unknown -asterisk.timezone", "value", *tz, "err", err)
			os.Exit(2)
		}
	}

	exp := collector.New(collector.Config{
		Address: *amiAddr, Username: *amiUser, Secret: secret, Timeout: *timeout, Location: loc,
		PJSIP: *pjsip, SIP: *sip, ConfBridge: *confbridge, PerEndpoint: *perEndpoint, Version: version,
	}, log)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           newMux(exp, *metricsPath),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("serving", "version", version, "address", *listen, "path", *metricsPath, "ami", *amiAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// validateMetricsPath rejects paths that would shadow the landing page or the
// health endpoint (or make the mux panic on a duplicate pattern).
func validateMetricsPath(p string) error {
	if !strings.HasPrefix(p, "/") || p == "/" || p == "/healthz" {
		return fmt.Errorf("%q must start with / and differ from / and /healthz", p)
	}
	return nil
}

// readSecret returns the AMI secret from a file or, failing that, the environment.
func readSecret(path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv("ASTERISK_AMI_SECRET"), nil
}

type scraper interface {
	Scrape(ctx context.Context) *expfmt.Set
}

func newMux(s scraper, metricsPath string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(metricsPath, func(w http.ResponseWriter, r *http.Request) {
		set := s.Scrape(r.Context())
		w.Header().Set("Content-Type", expfmt.ContentType)
		_, _ = set.WriteTo(w)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n")) // the exporter process is alive; Asterisk's state is asterisk_up
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>asterisk-exporter</title><h1>asterisk-exporter %s</h1><p><a href=%q>Metrics</a></p>\n", version, metricsPath)
	})
	return mux
}
