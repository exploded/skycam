package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/upload"
)

func newTest(t *testing.T) (*httptest.Server, Config) {
	t.Helper()
	cfg := Config{Token: "tok", User: "james", Password: "pw", DataDir: t.TempDir(), ReportDir: t.TempDir(),
		ReportBaseURL: "https://www.deepspaceplace.com/reports/skycam/", Loc: time.UTC,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, cfg
}

func do(t *testing.T, method, url, auth string, body []byte, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	switch auth {
	case "pi":
		req.Header.Set("Authorization", "Bearer tok")
	case "viewer":
		req.SetBasicAuth("james", "pw")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

var fakeJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}

func TestAuth(t *testing.T) {
	srv, _ := newTest(t)
	for _, c := range []struct{ method, path, auth string }{
		{"POST", "/api/frame", ""}, {"POST", "/api/frame", "viewer"}, {"GET", "/", ""}, {"GET", "/", "pi"},
	} {
		if r := do(t, c.method, srv.URL+c.path, c.auth, fakeJPEG, nil); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s as %q: %d", c.method, c.path, c.auth, r.StatusCode)
		}
	}
}

func TestFrameAndHealth(t *testing.T) {
	srv, cfg := newTest(t)
	if r := do(t, "GET", srv.URL+"/health/pi", "", nil, nil); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("pi health before contact: %d", r.StatusCode)
	}
	st, _ := json.Marshal(upload.StatusJSON{Time: time.Now(), Roof: "OPEN", Rain: "SAFE", Sky: "clear", Stars: 512})
	if r := do(t, "POST", srv.URL+"/api/frame", "pi", fakeJPEG, map[string]string{"X-Skycam-Status": string(st)}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("post frame: %d", r.StatusCode)
	}
	if r := do(t, "GET", srv.URL+"/health/pi", "", nil, nil); r.StatusCode != 200 {
		t.Fatalf("pi health after contact: %d", r.StatusCode)
	}
	r := do(t, "GET", srv.URL+"/", "viewer", nil, nil)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "OPEN") || !strings.Contains(string(b), "512 stars") {
		t.Fatalf("home %d: %s", r.StatusCode, b)
	}
	if got, _ := os.ReadFile(filepath.Join(cfg.DataDir, "latest.jpg")); !bytes.Equal(got, fakeJPEG) {
		t.Fatal("latest frame not saved")
	}
}

func TestDemandWakesOnViewer(t *testing.T) {
	srv, _ := newTest(t)
	got := make(chan bool, 1)
	go func() {
		r := do(t, "GET", srv.URL+"/api/demand?wait=10", "pi", nil, nil)
		var d struct{ Live bool }
		json.NewDecoder(r.Body).Decode(&d)
		got <- d.Live
	}()
	time.Sleep(200 * time.Millisecond)
	// A viewer opens the stream; the long poll should return live at once.
	req, _ := http.NewRequest("GET", srv.URL+"/live.mjpg", nil)
	req.SetBasicAuth("james", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case live := <-got:
		if !live {
			t.Fatal("demand said not live with a viewer watching")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not wake for the viewer")
	}
	// A live frame from the Pi reaches the viewer.
	do(t, "POST", srv.URL+"/api/live", "pi", fakeJPEG, nil)
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "--skycamframe") {
		t.Fatalf("stream: %q", buf[:n])
	}
}

func TestNightPublishesReport(t *testing.T) {
	srv, cfg := newTest(t)
	n, _ := json.Marshal(upload.NightJSON{Night: "20261023", Summary: "Clear all night", ReportHTML: "<html><body>report</body></html>",
		TimelapseURL: "https://bucket.s3.amazonaws.com/skycam/nights/20261023/allsky-20261023.mp4"})
	if r := do(t, "POST", srv.URL+"/api/night", "pi", n, nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("post night: %d", r.StatusCode)
	}
	if b, err := os.ReadFile(filepath.Join(cfg.ReportDir, "2026-10-23.html")); err != nil || !strings.Contains(string(b), "report") {
		t.Fatalf("report not published: %v", err)
	}
	idx, _ := os.ReadFile(filepath.Join(cfg.ReportDir, "index.html"))
	if !strings.Contains(string(idx), "2026-10-23.html") || !strings.Contains(string(idx), "Clear all night") {
		t.Fatalf("index: %s", idx)
	}
	bad, _ := json.Marshal(upload.NightJSON{Night: "../../x"})
	if r := do(t, "POST", srv.URL+"/api/night", "pi", bad, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad night name accepted: %d", r.StatusCode)
	}
}
