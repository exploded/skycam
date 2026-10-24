//go:build !(linux && (arm64 || amd64))

package asisdk

import (
	"errors"

	"github.com/exploded/skycam/internal/camera"
)

type Options struct {
	Offset    int
	Bandwidth int
	Flip      int
	Snap      bool
}

// Open is only implemented on 64-bit Linux, where the SDK's .so runs.
func Open(sdkPath string, opts Options) (camera.Camera, error) {
	return nil, errors.New("asisdk: the ZWO SDK driver needs 64-bit Linux; use camera.driver \"fake\" here")
}
