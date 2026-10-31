package session

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestNonOwnerCannotCloseActiveSession(t *testing.T) {
	owner := &SignalSocket{}
	other := &SignalSocket{}
	session := &PeerSession{owner: owner, done: make(chan struct{})}
	manager := &Manager{active: session}

	if err := manager.Handle(other, SignalMessage{Type: "close"}); err == nil {
		t.Fatal("close from a different WebSocket was accepted")
	}
	if manager.active != session {
		t.Fatal("close from a different WebSocket released the active session")
	}
	select {
	case <-session.done:
		t.Fatal("close from a different WebSocket closed the active session")
	default:
	}
}

func TestValidDisplaySizeUsesAreaAndMinimumBounds(t *testing.T) {
	for _, test := range []struct {
		name          string
		width, height int
		valid         bool
	}{
		{name: "minimum", width: 320, height: 200, valid: true},
		{name: "area boundary", width: 5_000, height: 5_000, valid: true},
		{name: "area exceeded", width: 5_004, height: 5_000, valid: false},
		{name: "too narrow", width: 319, height: 200, valid: false},
		{name: "too short", width: 320, height: 199, valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validDisplaySize(test.width, test.height); got != test.valid {
				t.Fatalf("validDisplaySize(%d, %d) = %t, want %t", test.width, test.height, got, test.valid)
			}
		})
	}
}

func TestPionAcceptsH264ReceiverCapabilityExtensions(t *testing.T) {
	offerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer offerer.Close()
	if _, err := offerer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := offerer.CreateDataChannel("input", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(offer.SDP, "\n")
	modified := false
	for i, line := range lines {
		if strings.HasPrefix(line, "a=fmtp:") && strings.Contains(line, "profile-level-id=42e01f") && strings.Contains(line, "packetization-mode=1") {
			lines[i] = strings.TrimRight(line, "\r") + ";max-fs=14400;max-mbps=864000\r"
			modified = true
		}
	}
	if !modified {
		t.Fatal("Pion offer had no Constrained Baseline H.264 format to extend")
	}
	offer.SDP = strings.Join(lines, "\n")

	mediaEngine := &webrtc.MediaEngine{}
	codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90_000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		PayloadType: 102,
	}
	if err := mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	answerer, err := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer answerer.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(codec.RTPCodecCapability, "desktop", "remotemac")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := answerer.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	if err := answerer.SetRemoteDescription(offer); err != nil {
		t.Fatalf("Pion rejected the capability-extended H.264 offer: %v", err)
	}
	if _, err := answerer.CreateAnswer(nil); err != nil {
		t.Fatalf("Pion could not answer the capability-extended H.264 offer: %v", err)
	}
}
