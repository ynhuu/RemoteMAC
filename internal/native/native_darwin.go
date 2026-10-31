//go:build darwin && cgo

package native

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -fblocks
#cgo LDFLAGS: -framework AppKit -framework CoreGraphics -framework CoreMedia -framework CoreVideo -framework ScreenCaptureKit -framework VideoToolbox -framework Foundation -framework IOKit -framework QuartzCore -lobjc
#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>
#include "VirtualDisplayBridge.h"
int rm_app_initialize(void);
void rm_app_run(void);
void rm_app_stop(void);
int rm_set_cursor_scale(float scale);
int rm_capture_start(uintptr_t handle, uint32_t screen_width, uint32_t screen_height, uint32_t width, uint32_t height, uint32_t fps, uint32_t bitrate, char *error, size_t error_len);
void rm_capture_get_display_size(uint32_t *width, uint32_t *height);
void rm_capture_stop(void);
int rm_capture_resize_display(uint32_t width, uint32_t height, char *error, size_t error_len);
int rm_capture_set_video(uintptr_t request_id, uint32_t width, uint32_t height, uint32_t fps, uint32_t bitrate, char *error, size_t error_len);
void rm_capture_request_keyframe(void);
void rm_input_pointer(int action, int button, double x, double y, uint32_t width, uint32_t height);
void rm_input_key(uint16_t keycode, int down);
void rm_input_mission_control(void);
void rm_input_text(const char *text);
void rm_input_release_all(void);
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

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
type ErrorHandler func(error)

var (
	handlers            sync.Map
	callbackErrors      sync.Map
	configResults       sync.Map
	nextID              atomic.Uint64
	cursorScaleWatchdog struct {
		sync.Mutex
		writer *os.File
	}
)

func Initialize() error {
	if C.rm_app_initialize() != 0 {
		return errors.New("初始化 AppKit 主线程失败")
	}
	if C.rm_set_cursor_scale(1) != 0 {
		log.Printf("初始化时恢复系统鼠标大小失败")
	}
	return nil
}

func RunMainLoop()  { C.rm_app_run() }
func StopMainLoop() { C.rm_app_stop() }

func StartWithDisplaySize(screenWidth, screenHeight, width, height, fps, bitrate int, handler FrameHandler, onError ErrorHandler) error {
	prepareCursorScaleWatchdog()
	id := nextID.Add(1)
	handlers.Store(id, handler)
	callbackErrors.Store(id, onError)
	started := make(chan error, 1)
	configResults.Store(id, started)
	var errorBuffer [512]C.char
	status := C.rm_capture_start(C.uintptr_t(id), C.uint32_t(screenWidth), C.uint32_t(screenHeight), C.uint32_t(width), C.uint32_t(height), C.uint32_t(fps), C.uint32_t(bitrate), &errorBuffer[0], C.size_t(len(errorBuffer)))
	if status != 0 {
		finishCursorScaleWatchdog()
		configResults.Delete(id)
		handlers.Delete(id)
		callbackErrors.Delete(id)
		return nativeError(&errorBuffer[0])
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case err := <-started:
		if err != nil {
			return fmt.Errorf("启动 macOS 视频采集：%w", err)
		}
		return nil
	case <-timer.C:
		configResults.Delete(id)
		return errors.New("等待 macOS 屏幕采集流与 VideoToolbox 编码器启动超时")
	}
}

func Stop() error {
	C.rm_capture_stop()
	finishCursorScaleWatchdog()
	handlers.Range(func(key, _ any) bool { handlers.Delete(key); callbackErrors.Delete(key); return true })
	return nil
}

func ActiveDisplaySize() (int, int) {
	var width, height C.uint32_t
	C.rm_capture_get_display_size(&width, &height)
	return int(width), int(height)
}

func WatchCursorScale(signal io.Reader) error {
	var state [1]byte
	n, err := io.ReadFull(signal, state[:])
	if n == 1 && state[0] == 1 {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if C.rm_set_cursor_scale(1) != 0 {
		return errors.New("异常退出后恢复系统鼠标大小失败")
	}
	return nil
}

func prepareCursorScaleWatchdog() {
	cursorScaleWatchdog.Lock()
	defer cursorScaleWatchdog.Unlock()
	if cursorScaleWatchdog.writer != nil {
		return
	}

	executable, err := os.Executable()
	if err != nil {
		log.Printf("启动鼠标恢复监护进程失败：%v", err)
		return
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		log.Printf("创建鼠标恢复监护通道失败：%v", err)
		return
	}
	command := exec.Command(executable)
	command.Stdin = reader
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.Env = []string{"REMOTEMAC_INTERNAL_CURSOR_WATCHDOG=1"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		log.Printf("启动鼠标恢复监护进程失败：%v", err)
		return
	}
	reader.Close()
	cursorScaleWatchdog.writer = writer
	go func() { _ = command.Wait() }()
}

func finishCursorScaleWatchdog() {
	cursorScaleWatchdog.Lock()
	defer cursorScaleWatchdog.Unlock()
	if cursorScaleWatchdog.writer != nil {
		_, _ = cursorScaleWatchdog.writer.Write([]byte{1})
		_ = cursorScaleWatchdog.writer.Close()
		cursorScaleWatchdog.writer = nil
	}
}

func ResizeVirtualDisplay(width, height int) error {
	var errorBuffer [512]C.char
	status := C.rm_capture_resize_display(C.uint32_t(width), C.uint32_t(height), &errorBuffer[0], C.size_t(len(errorBuffer)))
	if status == -2 {
		return fmt.Errorf("%w：%s", ErrDisplayRecoveryFailed, nativeError(&errorBuffer[0]))
	}
	if status != 0 {
		return nativeError(&errorBuffer[0])
	}
	return nil
}

func SetVideoConfig(width, height, fps, bitrate int) error {
	requestID := nextID.Add(1)
	result := make(chan error, 1)
	configResults.Store(requestID, result)
	var errorBuffer [512]C.char
	if C.rm_capture_set_video(C.uintptr_t(requestID), C.uint32_t(width), C.uint32_t(height), C.uint32_t(fps), C.uint32_t(bitrate), &errorBuffer[0], C.size_t(len(errorBuffer))) != 0 {
		configResults.Delete(requestID)
		return nativeError(&errorBuffer[0])
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		configResults.Delete(requestID)
		return errors.New("等待 macOS 视频采集配置超时")
	}
}

func RequestKeyframe() error { C.rm_capture_request_keyframe(); return nil }

func InjectPointer(action string, x, y float64, button, width, height int) error {
	if width <= 0 || height <= 0 {
		return errors.New("输入画布尺寸无效")
	}
	actionID := C.int(0)
	switch action {
	case "move":
		actionID = 0
	case "down":
		actionID = 1
	case "up":
		actionID = 2
	case "wheel":
		actionID = 3
	default:
		return fmt.Errorf("未知指针事件 %q", action)
	}
	C.rm_input_pointer(actionID, C.int(button), C.double(x), C.double(y), C.uint32_t(width), C.uint32_t(height))
	return nil
}

func InjectKey(keycode uint16, down bool) error {
	flag := C.int(0)
	if down {
		flag = 1
	}
	C.rm_input_key(C.uint16_t(keycode), flag)
	return nil
}

func InvokeMissionControl() error {
	C.rm_input_mission_control()
	return nil
}

func InjectText(text string) error {
	if strings.ContainsRune(text, '\x00') {
		return errors.New("文本包含非法空字符")
	}
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))
	C.rm_input_text(cText)
	return nil
}

func ReleaseAll() { C.rm_input_release_all() }

func nativeError(value *C.char) error {
	if value == nil {
		return errors.New("macOS 原生桌面桥接失败")
	}
	return errors.New(C.GoString(value))
}

//export rmGoFrame
func rmGoFrame(handle C.uintptr_t, bytes *C.uint8_t, length C.size_t, width C.uint32_t, height C.uint32_t, fps C.uint32_t, keyframe C.int, presentationNS C.int64_t) {
	callback, ok := handlers.Load(uint64(handle))
	if !ok || bytes == nil || length == 0 || length > 32<<20 {
		return
	}
	captureAt := time.Time{}
	if presentationNS >= 0 {
		captureAt = time.Unix(0, int64(presentationNS))
	}
	encodedAt := time.Now()
	data := C.GoBytes(unsafe.Pointer(bytes), C.int(length))
	callback.(FrameHandler)(Frame{
		AnnexB: data, EncodedAt: encodedAt, CaptureAt: captureAt, Width: uint32(width), Height: uint32(height),
		FPS: uint32(fps), Keyframe: keyframe != 0,
	})
}

//export rmGoError
func rmGoError(handle C.uintptr_t, message *C.char) {
	if message == nil {
		return
	}
	if callback, ok := callbackErrors.Load(uint64(handle)); ok {
		callback.(ErrorHandler)(errors.New(C.GoString(message)))
	}
}

//export rmGoVideoConfigComplete
func rmGoVideoConfigComplete(handle C.uintptr_t, message *C.char) {
	result, ok := configResults.LoadAndDelete(uint64(handle))
	if !ok {
		return
	}
	var err error
	if message != nil {
		err = errors.New(C.GoString(message))
	}
	result.(chan error) <- err
}
