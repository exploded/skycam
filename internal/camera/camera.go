// Package camera is the boundary between skycam and the hardware. The
// capture loop only ever sees Camera, so the ZWO SDK driver, the native USB
// driver and the fake can be swapped by config.
package camera

import (
	"context"
	"errors"
	"image"
	"math"
	"time"
)

// Info describes the connected camera.
type Info struct {
	Name        string
	Width       int
	Height      int
	PixelSizeUM float64
	BitDepth    int
	MinExposure time.Duration
	MaxExposure time.Duration
	MinGain     int
	MaxGain     int
}

// Settings are what the capture loop asks for on each frame.
type Settings struct {
	Exposure time.Duration
	Gain     int
}

// Frame is one 8-bit mono image plus how it was taken.
type Frame struct {
	Image    *image.Gray
	Start    time.Time // when the exposure started
	Exposure time.Duration
	Gain     int
	// TempC is the sensor temperature, NaN when the camera can't say.
	TempC float64
}

// Camera is a mono camera that takes single long exposures (snap mode) and
// can switch to a short-exposure video stream for the live view.
type Camera interface {
	Info() Info
	// Capture takes one exposure and blocks until the image is read out.
	Capture(ctx context.Context, s Settings) (Frame, error)
	// StartVideo switches to streaming; Capture must not be called until
	// StopVideo.
	StartVideo(s Settings) error
	// SetVideo changes exposure and gain while streaming.
	SetVideo(s Settings) error
	// VideoFrame waits for the next streamed frame.
	VideoFrame(ctx context.Context) (Frame, error)
	StopVideo() error
	Close() error
}

// ErrNoCamera means nothing is plugged in (or the driver can't see it).
var ErrNoCamera = errors.New("no camera found")

// NoTemp is the TempC of a frame from a camera without a sensor reading.
var NoTemp = math.NaN()
