//go:build (linux && (arm64 || amd64)) || (windows && amd64)

// Package asisdk drives a ZWO ASI camera through ZWO's closed-source
// libASICamera2.so, loaded at run time with purego, so skycam still builds
// with CGO_ENABLED=0 and cross-compiles from Windows. It also runs on
// 64-bit Windows against ASICamera2.dll, for bench tests with the camera
// on a PC.
//
// The structs mirror ASICamera2.h (SDK 1.41). C long is 8 bytes on 64-bit
// Linux and 4 on 64-bit Windows, so it is spelt clong (long_*.go). A 32-bit
// userland (armv7) is not supported.
package asisdk

import (
	"context"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/exploded/skycam/internal/camera"
)

// ASI_CONTROL_TYPE values used here.
const (
	ctlGain        = 0
	ctlExposure    = 1
	ctlGamma       = 2
	ctlOffset      = 5
	ctlBandwidth   = 6
	ctlTemperature = 8
	ctlFlip        = 9
	ctlHighSpeed   = 14
)

const (
	imgRAW8 = 0
	imgY8   = 3
)

// ASI_EXPOSURE_STATUS.
const (
	expIdle    = 0
	expWorking = 1
	expSuccess = 2
	expFailed  = 3
)

var errNames = []string{
	"SUCCESS", "INVALID_INDEX", "INVALID_ID", "INVALID_CONTROL_TYPE",
	"CAMERA_CLOSED", "CAMERA_REMOVED", "INVALID_PATH", "INVALID_FILEFORMAT",
	"INVALID_SIZE", "INVALID_IMGTYPE", "OUTOF_BOUNDARY", "TIMEOUT",
	"INVALID_SEQUENCE", "BUFFER_TOO_SMALL", "VIDEO_MODE_ACTIVE",
	"EXPOSURE_IN_PROGRESS", "GENERAL_ERROR", "INVALID_MODE",
}

type asiError struct {
	call string
	code int32
}

func (e asiError) Error() string {
	name := fmt.Sprintf("error %d", e.code)
	if int(e.code) < len(errNames) {
		name = "ASI_ERROR_" + errNames[e.code]
	}
	return e.call + ": " + name
}

func check(call string, code int32) error {
	if code == 0 {
		return nil
	}
	return asiError{call, code}
}

// cameraInfo is ASI_CAMERA_INFO.
type cameraInfo struct {
	Name                 [64]byte
	CameraID             int32
	MaxHeight            clong
	MaxWidth             clong
	IsColorCam           int32
	BayerPattern         int32
	SupportedBins        [16]int32
	SupportedVideoFormat [8]int32
	PixelSize            float64
	MechanicalShutter    int32
	ST4Port              int32
	IsCoolerCam          int32
	IsUSB3Host           int32
	IsUSB3Camera         int32
	ElecPerADU           float32
	BitDepth             int32
	IsTriggerCam         int32
	Unused               [16]byte
}

// controlCaps is ASI_CONTROL_CAPS.
type controlCaps struct {
	Name            [64]byte
	Description     [128]byte
	MaxValue        clong
	MinValue        clong
	DefaultValue    clong
	IsAutoSupported int32
	IsWritable      int32
	ControlType     int32
	Unused          [32]byte
}

// The SDK functions, bound by Open.
var (
	asiGetNumOfConnectedCameras func() int32
	asiGetCameraProperty        func(info unsafe.Pointer, index int32) int32
	asiOpenCamera               func(id int32) int32
	asiInitCamera               func(id int32) int32
	asiCloseCamera              func(id int32) int32
	asiGetNumOfControls         func(id int32, n unsafe.Pointer) int32
	asiGetControlCaps           func(id int32, index int32, caps unsafe.Pointer) int32
	asiGetControlValue          func(id int32, ctl int32, value unsafe.Pointer, auto unsafe.Pointer) int32
	asiSetControlValue          func(id int32, ctl int32, value clong, auto int32) int32
	asiSetROIFormat             func(id int32, w, h, bin, imgType int32) int32
	asiStartExposure            func(id int32, dark int32) int32
	asiStopExposure             func(id int32) int32
	asiGetExpStatus             func(id int32, status unsafe.Pointer) int32
	asiGetDataAfterExp          func(id int32, buf unsafe.Pointer, size clong) int32
	asiStartVideoCapture        func(id int32) int32
	asiStopVideoCapture         func(id int32) int32
	asiGetVideoData             func(id int32, buf unsafe.Pointer, size clong, waitMs int32) int32
)

var loadOnce sync.Once
var loadErr error

func load(path string) error {
	loadOnce.Do(func() {
		lib, err := openLibrary(path)
		if err != nil {
			loadErr = fmt.Errorf("loading %s: %w", path, err)
			return
		}
		bind := func(fn any, name string) {
			if loadErr != nil {
				return
			}
			defer func() {
				if r := recover(); r != nil {
					loadErr = fmt.Errorf("binding %s: %v", name, r)
				}
			}()
			purego.RegisterLibFunc(fn, lib, name)
		}
		bind(&asiGetNumOfConnectedCameras, "ASIGetNumOfConnectedCameras")
		bind(&asiGetCameraProperty, "ASIGetCameraProperty")
		bind(&asiOpenCamera, "ASIOpenCamera")
		bind(&asiInitCamera, "ASIInitCamera")
		bind(&asiCloseCamera, "ASICloseCamera")
		bind(&asiGetNumOfControls, "ASIGetNumOfControls")
		bind(&asiGetControlCaps, "ASIGetControlCaps")
		bind(&asiGetControlValue, "ASIGetControlValue")
		bind(&asiSetControlValue, "ASISetControlValue")
		bind(&asiSetROIFormat, "ASISetROIFormat")
		bind(&asiStartExposure, "ASIStartExposure")
		bind(&asiStopExposure, "ASIStopExposure")
		bind(&asiGetExpStatus, "ASIGetExpStatus")
		bind(&asiGetDataAfterExp, "ASIGetDataAfterExp")
		bind(&asiStartVideoCapture, "ASIStartVideoCapture")
		bind(&asiStopVideoCapture, "ASIStopVideoCapture")
		bind(&asiGetVideoData, "ASIGetVideoData")
	})
	return loadErr
}

// Options are applied once at open.
type Options struct {
	Offset    int
	Bandwidth int
	Flip      int
	// Snap uses ASIStartExposure for stills. The default (false) takes each
	// still as a one-frame video capture, which is what allsky does on the
	// ASI120: its snap mode is unreliable on USB 2 cameras.
	Snap bool
}

type Camera struct {
	id   int32
	info camera.Info
	opts Options

	mu       sync.Mutex
	buf      []byte
	video    bool
	settings camera.Settings
}

// Open loads the SDK from sdkPath and opens the first camera.
func Open(sdkPath string, opts Options) (camera.Camera, error) {
	if err := load(sdkPath); err != nil {
		return nil, err
	}
	if asiGetNumOfConnectedCameras() < 1 {
		return nil, camera.ErrNoCamera
	}
	var ci cameraInfo
	if err := check("ASIGetCameraProperty", asiGetCameraProperty(unsafe.Pointer(&ci), 0)); err != nil {
		return nil, err
	}
	id := ci.CameraID
	if err := check("ASIOpenCamera", asiOpenCamera(id)); err != nil {
		return nil, err
	}
	if err := check("ASIInitCamera", asiInitCamera(id)); err != nil {
		asiCloseCamera(id)
		return nil, err
	}
	c := &Camera{id: id, opts: opts}
	c.info = camera.Info{
		Name:        cstr(ci.Name[:]),
		Width:       int(ci.MaxWidth),
		Height:      int(ci.MaxHeight),
		PixelSizeUM: ci.PixelSize,
		BitDepth:    8,
	}
	if err := c.readCaps(); err != nil {
		c.Close()
		return nil, err
	}
	// RAW8 on a mono sensor is the plain 8-bit image. The ASI120 needs
	// w×h to be a multiple of 1024; the full 1280×960 frame is.
	w, h := c.info.Width&^7, c.info.Height&^1
	if err := check("ASISetROIFormat", asiSetROIFormat(id, int32(w), int32(h), 1, imgRAW8)); err != nil {
		c.Close()
		return nil, err
	}
	c.info.Width, c.info.Height = w, h
	c.buf = make([]byte, w*h)

	for _, s := range []struct {
		ctl int32
		v   clong
	}{
		{ctlBandwidth, clong(opts.Bandwidth)},
		{ctlOffset, clong(opts.Offset)},
		{ctlFlip, clong(opts.Flip)},
		{ctlHighSpeed, 0},
	} {
		// Not every camera has every control; a refusal here is not fatal.
		asiSetControlValue(id, s.ctl, s.v, 0)
	}
	return c, nil
}

func (c *Camera) readCaps() error {
	var n int32
	if err := check("ASIGetNumOfControls", asiGetNumOfControls(c.id, unsafe.Pointer(&n))); err != nil {
		return err
	}
	for i := int32(0); i < n; i++ {
		var cc controlCaps
		if asiGetControlCaps(c.id, i, unsafe.Pointer(&cc)) != 0 {
			continue
		}
		switch cc.ControlType {
		case ctlGain:
			c.info.MinGain, c.info.MaxGain = int(cc.MinValue), int(cc.MaxValue)
		case ctlExposure:
			c.info.MinExposure = time.Duration(cc.MinValue) * time.Microsecond
			c.info.MaxExposure = time.Duration(cc.MaxValue) * time.Microsecond
		}
	}
	return nil
}

func (c *Camera) Info() camera.Info { return c.info }

func (c *Camera) apply(s camera.Settings) error {
	us := s.Exposure.Microseconds()
	if err := check("set exposure", asiSetControlValue(c.id, ctlExposure, clong(us), 0)); err != nil {
		return err
	}
	return check("set gain", asiSetControlValue(c.id, ctlGain, clong(s.Gain), 0))
}

func (c *Camera) temp() float64 {
	var v clong
	var auto int32
	if asiGetControlValue(c.id, ctlTemperature, unsafe.Pointer(&v), unsafe.Pointer(&auto)) != 0 {
		return camera.NoTemp
	}
	return float64(v) / 10
}

// waitMs is how long ASIGetVideoData may block: the SDK's own advice is
// twice the exposure plus 500 ms.
func waitMs(exp time.Duration) int32 {
	return int32(exp.Milliseconds()*2 + 500)
}

func (c *Camera) Capture(ctx context.Context, s camera.Settings) (camera.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.video {
		return camera.Frame{}, fmt.Errorf("asisdk: capture while video is running")
	}
	if err := c.apply(s); err != nil {
		return camera.Frame{}, err
	}
	start := time.Now()
	var err error
	if c.opts.Snap {
		err = c.snap(ctx, s.Exposure)
	} else {
		err = c.oneVideoFrame(s.Exposure)
	}
	if err != nil {
		return camera.Frame{}, err
	}
	return c.frame(start, s), nil
}

func (c *Camera) snap(ctx context.Context, exp time.Duration) error {
	if err := check("ASIStartExposure", asiStartExposure(c.id, 0)); err != nil {
		return err
	}
	deadline := time.Now().Add(exp + 15*time.Second)
	// Sleep through most of the exposure, then poll.
	if exp > 50*time.Millisecond {
		select {
		case <-time.After(exp - 20*time.Millisecond):
		case <-ctx.Done():
			asiStopExposure(c.id)
			return ctx.Err()
		}
	}
	for {
		var st int32
		if err := check("ASIGetExpStatus", asiGetExpStatus(c.id, unsafe.Pointer(&st))); err != nil {
			return err
		}
		switch st {
		case expSuccess:
			return check("ASIGetDataAfterExp", asiGetDataAfterExp(c.id, unsafe.Pointer(&c.buf[0]), clong(len(c.buf))))
		case expFailed:
			return fmt.Errorf("asisdk: exposure failed")
		case expIdle:
			return fmt.Errorf("asisdk: exposure went idle without an image")
		}
		if time.Now().After(deadline) {
			asiStopExposure(c.id)
			return fmt.Errorf("asisdk: exposure timed out")
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			asiStopExposure(c.id)
			return ctx.Err()
		}
	}
}

// oneVideoFrame takes a still the way allsky does on ZWO cameras: start
// video, read one frame, stop.
func (c *Camera) oneVideoFrame(exp time.Duration) error {
	if err := check("ASIStartVideoCapture", asiStartVideoCapture(c.id)); err != nil {
		return err
	}
	err := check("ASIGetVideoData", asiGetVideoData(c.id, unsafe.Pointer(&c.buf[0]), clong(len(c.buf)), waitMs(exp)))
	asiStopVideoCapture(c.id)
	return err
}

func (c *Camera) frame(start time.Time, s camera.Settings) camera.Frame {
	img := image.NewGray(image.Rect(0, 0, c.info.Width, c.info.Height))
	copy(img.Pix, c.buf)
	return camera.Frame{Image: img, Start: start, Exposure: s.Exposure, Gain: s.Gain, TempC: c.temp()}
}

func (c *Camera) StartVideo(s camera.Settings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.apply(s); err != nil {
		return err
	}
	if err := check("ASIStartVideoCapture", asiStartVideoCapture(c.id)); err != nil {
		return err
	}
	c.video = true
	c.settings = s
	return nil
}

func (c *Camera) SetVideo(s camera.Settings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.apply(s); err != nil {
		return err
	}
	c.settings = s
	return nil
}

func (c *Camera) VideoFrame(ctx context.Context) (camera.Frame, error) {
	if err := ctx.Err(); err != nil {
		return camera.Frame{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.video {
		return camera.Frame{}, fmt.Errorf("asisdk: video not running")
	}
	s := c.settings
	start := time.Now()
	if err := check("ASIGetVideoData", asiGetVideoData(c.id, unsafe.Pointer(&c.buf[0]), clong(len(c.buf)), waitMs(s.Exposure))); err != nil {
		return camera.Frame{}, err
	}
	return c.frame(start, s), nil
}

func (c *Camera) StopVideo() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.video {
		return nil
	}
	c.video = false
	return check("ASIStopVideoCapture", asiStopVideoCapture(c.id))
}

func (c *Camera) Close() error {
	c.StopVideo()
	return check("ASICloseCamera", asiCloseCamera(c.id))
}

func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
