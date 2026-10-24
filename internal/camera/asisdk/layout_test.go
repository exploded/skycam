//go:build (linux && (arm64 || amd64)) || (windows && amd64)

package asisdk

import (
	"runtime"
	"testing"
	"unsafe"
)

// The SDK writes these structs through a pointer, so a size or offset
// that differs from the C compiler's corrupts memory instead of failing.
// Expected values are sizeof/offsetof from ASICamera2.h under LP64 (Linux)
// and LLP64 (Windows).
func TestStructLayout(t *testing.T) {
	type want struct{ info, caps, pixelSize, maxValue uintptr }
	w := want{info: 248, caps: 264, pixelSize: 192, maxValue: 192}
	if runtime.GOOS == "windows" {
		w = want{info: 240, caps: 248, pixelSize: 184, maxValue: 192}
	}
	var ci cameraInfo
	var cc controlCaps
	got := want{unsafe.Sizeof(ci), unsafe.Sizeof(cc), unsafe.Offsetof(ci.PixelSize), unsafe.Offsetof(cc.MaxValue)}
	if got != w {
		t.Fatalf("layout %+v, want %+v", got, w)
	}
}
