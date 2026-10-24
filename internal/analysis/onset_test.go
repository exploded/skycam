package analysis

import (
	"math"
	"testing"
)

func TestCloudOnset(t *testing.T) {
	nan := math.NaN()
	for _, tc := range []struct {
		name  string
		cover []float64
		want  int
	}{
		{"none", nil, -1},
		{"clear all night", []float64{0.1, 0.2, 0.1, 0.3}, -1},
		{"onset", []float64{0.1, 0.2, 0.6, 0.7, 0.9, 0.95}, 2},
		{"threshold counts", []float64{0.5, 0.5, 0.5}, 0},
		{"a blip is not onset", []float64{0.1, 0.8, 0.9, 0.2, 0.1}, -1},
		{"unknown breaks a run", []float64{0.8, 0.9, nan, 0.9, 0.1}, -1},
		{"onset after the roof opens", []float64{nan, nan, 0.1, 0.7, 0.8, 0.9}, 3},
	} {
		if got := CloudOnset(tc.cover); got != tc.want {
			t.Errorf("%s: CloudOnset = %d, want %d", tc.name, got, tc.want)
		}
	}
}
