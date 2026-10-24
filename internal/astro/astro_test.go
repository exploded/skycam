package astro

import (
	"math"
	"testing"
	"time"
)

const lat, lon = -37.0, 145.0

func mel(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Australia/Melbourne")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestSunAltitude(t *testing.T) {
	loc := mel(t)
	for _, tc := range []struct {
		name string
		at   time.Time
		want float64
		tol  float64
	}{
		// Solar noon in late October: 90 − (37.8 − 12.0) ≈ 64°.
		{"noon", time.Date(2026, 10, 24, 13, 6, 0, 0, loc), 64.0, 1.0},
		// allsky's first night frame (sun angle −6°) on 2026-10-23.
		{"allsky start", time.Date(2026, 10, 23, 20, 10, 25, 0, loc), -6.0, 1.5},
		// Midnight, well below the horizon.
		{"midnight", time.Date(2026, 10, 24, 0, 30, 0, 0, loc), -40, 10},
	} {
		got := SunAltitude(tc.at, lat, lon)
		if math.Abs(got-tc.want) > tc.tol {
			t.Errorf("%s: altitude %.2f, want %.1f ± %.1f", tc.name, got, tc.want, tc.tol)
		}
	}
}

func TestMoonIllumination(t *testing.T) {
	// New moon 2024-01-11 11:57 UTC, full moon 2024-01-25 17:54 UTC.
	if k := MoonIllumination(time.Date(2024, 1, 11, 12, 0, 0, 0, time.UTC)); k > 0.02 {
		t.Errorf("new moon illumination %.3f", k)
	}
	if k := MoonIllumination(time.Date(2024, 1, 25, 18, 0, 0, 0, time.UTC)); k < 0.98 {
		t.Errorf("full moon illumination %.3f", k)
	}
	if k := MoonIllumination(time.Date(2024, 1, 18, 3, 0, 0, 0, time.UTC)); math.Abs(k-0.5) > 0.1 {
		t.Errorf("first quarter illumination %.3f", k)
	}
}

func TestMoonAltitudeRange(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	lo, hi := 90.0, -90.0
	for h := 0; h < 24*30; h++ {
		a := MoonAltitude(start.Add(time.Duration(h)*time.Hour), lat, lon)
		lo, hi = min(lo, a), max(hi, a)
	}
	// From −37.8°, the moon peaks around 90 − 37.8 + 28.6 ≈ 81° at most.
	if hi < 40 || hi > 85 || lo > -40 {
		t.Errorf("moon altitude range %.1f..%.1f looks wrong", lo, hi)
	}
}

func TestNightDate(t *testing.T) {
	loc := mel(t)
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 10, 23, 20, 10, 0, 0, loc), "20261023"},
		{time.Date(2026, 10, 24, 5, 56, 0, 0, loc), "20261023"},
		{time.Date(2026, 10, 24, 12, 0, 0, 0, loc), "20261024"},
	} {
		if got := NightDate(tc.at, loc); got != tc.want {
			t.Errorf("NightDate(%v) = %s, want %s", tc.at, got, tc.want)
		}
	}
}
