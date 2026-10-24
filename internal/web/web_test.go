package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/capture"
	"github.com/exploded/skycam/internal/config"
)

func testServer(t *testing.T) (*httptest.Server, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ImagesUser, cfg.ImagesPassword = "skyq", "secret"
	night := filepath.Join(cfg.ImagesDir(), "20261023")
	os.MkdirAll(night, 0o755)
	for _, n := range []string{"image-20261023201025.jpg", "image-20261024003005.jpg", "notes.txt"} {
		os.WriteFile(filepath.Join(night, n), []byte("jpeg"), 0o644)
	}
	s := &Server{Cfg: cfg, Loc: time.UTC, Hub: capture.NewHub(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Camera: "Test"}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, cfg
}

func get(t *testing.T, url string, auth bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if auth {
		req.SetBasicAuth("skyq", "secret")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestImagesLikeAllsky checks the contract skyq's fetcher depends on:
// basic auth, and listings whose links match image-\d{14}\.jpg.
func TestImagesLikeAllsky(t *testing.T) {
	srv, _ := testServer(t)
	if code, _ := get(t, srv.URL+"/images/20261023/", false); code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", code)
	}
	code, body := get(t, srv.URL+"/images/", true)
	if code != 200 || !strings.Contains(body, `href="20261023/"`) {
		t.Fatalf("root listing %d: %s", code, body)
	}
	code, body = get(t, srv.URL+"/images/20261023/", true)
	// skyq de-duplicates: each name appears in the href and the link text.
	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`image-\d{14}\.jpg`).FindAllString(body, -1) {
		got[m] = true
	}
	if code != 200 || len(got) != 2 || strings.Contains(body, "notes.txt") {
		t.Fatalf("night listing %d: %v\n%s", code, got, body)
	}
	if code, body = get(t, srv.URL+"/images/20261023/image-20261023201025.jpg", true); code != 200 || body != "jpeg" {
		t.Fatalf("file %d %q", code, body)
	}
	for _, p := range []string{"/images/20261023/notes.txt", "/images/../skycam.db", "/images/20261023/../../skycam.db"} {
		if code, _ := get(t, srv.URL+p, true); code == 200 {
			t.Errorf("%s served", p)
		}
	}
}

func TestPagesRender(t *testing.T) {
	srv, _ := testServer(t)
	for _, p := range []string{"/", "/partials/latest", "/live", "/nights", "/health"} {
		if code, body := get(t, srv.URL+p, false); code != 200 {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	if code, _ := get(t, srv.URL+"/api/status", false); code != http.StatusServiceUnavailable {
		t.Errorf("status before first frame: %d", code)
	}
}
