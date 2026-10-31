package session

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type SignalMessage struct {
	Type          string  `json:"type"`
	SDP           string  `json:"sdp,omitempty"`
	Candidate     *string `json:"candidate,omitempty"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLine      *uint16 `json:"sdpMLineIndex,omitempty"`
	Width         int     `json:"width,omitempty"`
	Height        int     `json:"height,omitempty"`
	DisplayWidth  int     `json:"displayWidth,omitempty"`
	DisplayHeight int     `json:"displayHeight,omitempty"`
	EncodeWidth   int     `json:"encodeWidth,omitempty"`
	EncodeHeight  int     `json:"encodeHeight,omitempty"`
	FPS           int     `json:"fps,omitempty"`
	Bitrate       int     `json:"bitrate,omitempty"`
	MapVersion    int     `json:"mapVersion,omitempty"`
	State         string  `json:"state,omitempty"`
	Error         string  `json:"error,omitempty"`
	Fatal         bool    `json:"fatal,omitempty"`
	Retryable     bool    `json:"retryable,omitempty"`
}

type SignalSocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func NewSignalSocket(conn *websocket.Conn) *SignalSocket { return &SignalSocket{conn: conn} }

func (s *SignalSocket) Send(message SignalMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return s.conn.WriteJSON(message)
}

// Kick sends a fatal session-replacement notice before closing the signaling
// connection. The short write deadline keeps a stalled old client from
// delaying the new session handoff.
func (s *SignalSocket) Kick(message SignalMessage) {
	if s == nil || s.conn == nil {
		return
	}
	s.mu.Lock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_ = s.conn.WriteJSON(message)
	s.mu.Unlock()
	_ = s.conn.Close()
}

func (s *SignalSocket) Close() error {
	// Gorilla permits Close concurrently with writes. Do not wait for a blocked
	// writer: closing the transport must unblock it during session cleanup.
	return s.conn.Close()
}
