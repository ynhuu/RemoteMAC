package session

import (
	"math"
	"strings"
	"testing"
)

func TestValidInputRequiresCurrentMapAndValidCoordinates(t *testing.T) {
	base := inputEvent{Type: "pointer", Action: "down", X: 640, Y: 360, Button: 0, Width: 1280, Height: 720, MapVersion: 3}
	if !validInput(base, 3) {
		t.Fatal("valid current-map pointer event was rejected")
	}
	if validInput(base, 4) {
		t.Fatal("stale pointer event was accepted")
	}
	base.X = math.NaN()
	if validInput(base, 3) {
		t.Fatal("non-finite coordinate was accepted")
	}
	base.X = 1281
	if validInput(base, 3) {
		t.Fatal("out-of-bounds coordinate was accepted")
	}
}

func TestValidInputLimitsActionsKeysAndText(t *testing.T) {
	if !validInput(inputEvent{Type: "pointer", Action: "wheel", X: -80, Y: 120, Width: 1280, Height: 720}, 0) {
		t.Fatal("valid wheel event was rejected")
	}
	if validInput(inputEvent{Type: "pointer", Action: "exec", Width: 1280, Height: 720}, 0) {
		t.Fatal("unknown pointer action was accepted")
	}
	if validInput(inputEvent{Type: "key", KeyCode: 255}, 0) {
		t.Fatal("out-of-range key code was accepted")
	}
	if !validInput(inputEvent{Type: "text", Text: "你好，RemoteMAC"}, 0) {
		t.Fatal("valid Unicode text was rejected")
	}
	if validInput(inputEvent{Type: "text", Text: strings.Repeat("a", (16<<10)+1)}, 0) {
		t.Fatal("oversized text was accepted")
	}
	if !validInput(inputEvent{Type: "pointer", Action: "move", X: 4_999, Y: 4_999, Width: 5_000, Height: 5_000}, 0) {
		t.Fatal("valid high-resolution pointer canvas was rejected")
	}
	if validInput(inputEvent{Type: "pointer", Action: "move", X: 8_999, Y: 2_999, Width: 9_000, Height: 3_000}, 0) {
		t.Fatal("pointer canvas exceeding the display area was accepted")
	}
}

func TestScalePointerFromEncodedFrameToDesktopPixels(t *testing.T) {
	event := inputEvent{Action: "move", X: 640, Y: 360, Width: 1280, Height: 720}
	x, y := scalePointer(event, 2560, 1440)
	if x != 1280 || y != 720 {
		t.Fatalf("scaled pointer = %.1f, %.1f, want 1280, 720", x, y)
	}
	event.Action = "wheel"
	x, y = scalePointer(event, 2560, 1440)
	if x != 640 || y != 360 {
		t.Fatalf("wheel delta should not be scaled: %.1f, %.1f", x, y)
	}
}

func TestLatestPointerMoveReplacesStaleCoordinates(t *testing.T) {
	queue := make(chan inputEvent, 1)
	enqueueLatestPointerMove(queue, inputEvent{Type: "pointer", Action: "move", X: 10, Y: 20})
	enqueueLatestPointerMove(queue, inputEvent{Type: "pointer", Action: "move", X: 30, Y: 40})
	latest := <-queue
	if latest.X != 30 || latest.Y != 40 {
		t.Fatalf("queued pointer move = (%.0f, %.0f), want newest coordinates (30, 40)", latest.X, latest.Y)
	}
}
