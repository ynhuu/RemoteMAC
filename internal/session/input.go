package session

import (
	"math"
	"unicode/utf8"
)

type inputEvent struct {
	Type       string  `json:"type"`
	Action     string  `json:"action"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     int     `json:"button"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	MapVersion int     `json:"mapVersion"`
	KeyCode    uint16  `json:"keyCode"`
	Down       bool    `json:"down"`
	Text       string  `json:"text"`
}

func validInput(event inputEvent, mapVersion int32) bool {
	if event.MapVersion != int(mapVersion) {
		return false
	}
	switch event.Type {
	case "pointer":
		if event.Width < 1 || event.Height < 1 || event.Width > maximumDisplayArea/event.Height ||
			!finite(event.X) || !finite(event.Y) {
			return false
		}
		switch event.Action {
		case "move":
			return event.Button >= -1 && event.Button <= 2 && event.X >= 0 && event.X <= float64(event.Width) && event.Y >= 0 && event.Y <= float64(event.Height)
		case "down", "up":
			return event.Button >= 0 && event.Button <= 2 && event.X >= 0 && event.X <= float64(event.Width) && event.Y >= 0 && event.Y <= float64(event.Height)
		case "wheel":
			return math.Abs(event.X) <= 10_000 && math.Abs(event.Y) <= 10_000
		default:
			return false
		}
	case "key":
		return event.KeyCode <= 127
	case "text":
		return len(event.Text) > 0 && len(event.Text) <= 16<<10 && utf8.ValidString(event.Text)
	case "missionControl":
		return true
	default:
		return false
	}
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func enqueueLatestPointerMove(queue chan inputEvent, event inputEvent) {
	select {
	case queue <- event:
		return
	default:
	}
	select {
	case <-queue:
	default:
	}
	select {
	case queue <- event:
	default:
	}
}

func scalePointer(event inputEvent, width, height int) (float64, float64) {
	if event.Action == "wheel" || event.Width <= 0 || event.Height <= 0 {
		return event.X, event.Y
	}
	return event.X * float64(width) / float64(event.Width),
		event.Y * float64(height) / float64(event.Height)
}
