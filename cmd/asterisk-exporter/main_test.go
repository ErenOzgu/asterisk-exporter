package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ErenOzgu/asterisk-exporter/internal/expfmt"
)

type fakeScraper struct{ calls int }

func (f *fakeScraper) Scrape(context.Context) *expfmt.Set {
	f.calls++
	s := expfmt.New()
	s.Add("asterisk_up", "Up.", expfmt.Gauge, 1)
	return s
}

func TestMetricsEndpoint(t *testing.T) {
	f := &fakeScraper{}
	srv := httptest.NewServer(newMux(f, "/metrics"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != expfmt.ContentType {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "asterisk_up 1") || f.calls != 1 {
		t.Errorf("body %q, scrapes %d", body, f.calls)
	}
}

func TestHealthzDoesNotScrape(t *testing.T) {
	f := &fakeScraper{}
	srv := httptest.NewServer(newMux(f, "/metrics"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || f.calls != 0 {
		t.Errorf("status %d, scrapes %d (healthz must not touch Asterisk)", resp.StatusCode, f.calls)
	}
}

func TestLandingPageAndNotFound(t *testing.T) {
	srv := httptest.NewServer(newMux(&fakeScraper{}, "/custom"))
	defer srv.Close()
	for path, want := range map[string]int{"/": 200, "/custom": 200, "/metrics": 404, "/nope": 404} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestReadSecret(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	if err := os.WriteFile(file, []byte("  from-file \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASTERISK_AMI_SECRET", "from-env")
	if got, err := readSecret(file); err != nil || got != "from-file" {
		t.Errorf("file: %q, %v", got, err)
	}
	if got, err := readSecret(""); err != nil || got != "from-env" {
		t.Errorf("env: %q, %v", got, err)
	}
	if _, err := readSecret(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file must be an error")
	}
}

func TestValidateMetricsPath(t *testing.T) {
	for p, ok := range map[string]bool{"/metrics": true, "/a/b": true, "metrics": false, "/": false, "/healthz": false, "": false} {
		if (validateMetricsPath(p) == nil) != ok {
			t.Errorf("validateMetricsPath(%q) ok=%v, want %v", p, !ok, ok)
		}
	}
}
