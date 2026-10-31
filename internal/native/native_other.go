//go:build !darwin || !cgo

package native

import (
	"errors"
	"io"
	"time"
)

var errUnsupported = errors.New("RemoteMAC 原生桌面桥接仅支持启用 CGO 的 macOS")

type Frame struct {
	AnnexB    []byte
	EncodedAt time.Time
	CaptureAt time.Time
	Width     uint32
	Height    uint32
	FPS       uint32
	Keyframe  bool
}

type FrameHandler func(Frame)

func Initialize() error { return errUnsupported }
func RunMainLoop()      {}
func StopMainLoop()     {}
func StartWithDisplaySize(int, int, int, int, int, int, FrameHandler, func(error)) error {
	return errUnsupported
}
func Stop() error                                                 { return nil }
func ActiveDisplaySize() (int, int)                               { return 0, 0 }
func WatchCursorScale(io.Reader) error                            { return nil }
func ResizeVirtualDisplay(int, int) error                         { return errUnsupported }
func SetVideoConfig(int, int, int, int) error                     { return errUnsupported }
func RequestKeyframe() error                                      { return errUnsupported }
func InjectPointer(string, float64, float64, int, int, int) error { return errUnsupported }
func InjectKey(uint16, bool) error                                { return errUnsupported }
func InvokeMissionControl() error                                 { return errUnsupported }
func InjectText(string) error                                     { return errUnsupported }
func ReleaseAll()                                                 {}
