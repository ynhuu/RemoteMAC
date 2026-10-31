package netshape

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/transport/v5/stdnet"
)

func TestShaperPacesDatagramsAtConfiguredRate(t *testing.T) {
	shaper := NewShaper()
	if err := shaper.SetRateBPS(80_000); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if err := shaper.wait(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Fatalf("500-byte datagram at 80 kbps took %s, want about 50 ms", elapsed)
	}
}

func TestChangingRateReleasesQueuedDatagram(t *testing.T) {
	shaper := NewShaper()
	if err := shaper.SetRateBPS(80_000); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- shaper.wait(context.Background(), 5_000) }()
	time.Sleep(20 * time.Millisecond)

	started := time.Now()
	if err := shaper.SetRateBPS(0); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("disabling the shaper took %s to release the packet", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("disabling the shaper did not release the queued packet")
	}
}

func TestWrappedUDPConnectionPacesOutboundDatagrams(t *testing.T) {
	receiver, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	base, err := stdnet.NewNet()
	if err != nil {
		t.Fatal(err)
	}
	shaper := NewShaper()
	if err := shaper.SetRateBPS(80_000); err != nil {
		t.Fatal(err)
	}
	conn, err := Wrap(base, shaper).ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	packet := make([]byte, 500)
	started := time.Now()
	for range 2 {
		if _, err := conn.WriteTo(packet, receiver.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed < 80*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Fatalf("two wrapped 500-byte datagrams at 80 kbps took %s", elapsed)
	}
	_ = receiver.SetReadDeadline(time.Now().Add(time.Second))
	for range 2 {
		if _, _, err := receiver.ReadFrom(make([]byte, 1_024)); err != nil {
			t.Fatalf("shaped UDP datagram was not delivered: %v", err)
		}
	}
}

func TestShaperRejectsOutOfRangeRate(t *testing.T) {
	shaper := NewShaper()
	for _, rate := range []int{-1, maxRateBPS + 1} {
		if err := shaper.SetRateBPS(rate); err != ErrInvalidRate {
			t.Errorf("SetRateBPS(%d) error = %v, want %v", rate, err, ErrInvalidRate)
		}
	}
}
