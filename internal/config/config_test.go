package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The shipped example must load: Load rejects unknown keys, so a renamed
// field would otherwise only show up on the Pi.
func TestExampleLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exposure.Night.MaxExposure.D() != 5*time.Second || cfg.Camera.Driver != "sdk" {
		t.Errorf("unexpected values: %+v", cfg.Exposure.Night)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"camera": {"drvier": "fake"}}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("typo accepted")
	}
}

func TestPartialFileKeepsDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"listen": ":8080"}`), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.RetentionNights != 14 || cfg.Exposure.Night.MinGain != 50 {
		t.Errorf("defaults lost: %+v", cfg)
	}
}
