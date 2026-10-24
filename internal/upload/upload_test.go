package upload

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/exploded/skycam/internal/config"
	"github.com/exploded/skycam/internal/night"
)

// The keys must match allsky's layout in deepspaceplace-allsky, so the
// archive allsky started carries on without a gap.
func TestProductKeysMatchAllsky(t *testing.T) {
	cfg := config.Default()
	cfg.S3.Enabled, cfg.S3.AccessKeyID, cfg.S3.SecretAccessKey = true, "AKIA", "secret"
	u, err := New(context.Background(), cfg, time.UTC, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	res := night.Result{Night: "20261023", Timelapse: "allsky-20261023.mp4", Keogram: "keogram-20261023.jpg",
		Startrails: "startrails-20261023.jpg", Thumbnail: "thumbnail-20261023.jpg", Report: "report.html"}
	want := map[string]string{
		"allsky-20261023.mp4":     "allsky/videos/allsky-20261023.mp4",
		"thumbnail-20261023.jpg":  "allsky/videos/thumbnails/allsky-20261023.jpg",
		"keogram-20261023.jpg":    "allsky/keograms/keogram-20261023.jpg",
		"startrails-20261023.jpg": "allsky/startrails/startrails-20261023.jpg",
	}
	got := u.productKeys(res)
	if len(got) != len(want) {
		t.Fatalf("keys %v", got)
	}
	for f, k := range want {
		if got[f] != k {
			t.Errorf("%s -> %q, want %q", f, got[f], k)
		}
	}
	if url := u.TimelapseURL(res); url != "https://deepspaceplace-allsky.s3.ap-southeast-2.amazonaws.com/allsky/videos/allsky-20261023.mp4" {
		t.Errorf("timelapse URL %s", url)
	}
}
