package media

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

type testRTPWriter struct {
	writes atomic.Int32
	write  func()
}

func (w *testRTPWriter) Write(*rtp.Header, []byte, interceptor.Attributes) (int, error) {
	w.writes.Add(1)
	if w.write != nil {
		w.write()
	}
	return 0, nil
}

func makeTestPacer(onDrop func(string)) *BoundedPacer {
	return &BoundedPacer{
		writers: make(map[uint32]interceptor.RTPWriter), targetBPS: 2_000_000,
		waitingIDR: true, done: make(chan struct{}), wake: make(chan struct{}, 1), onDrop: onDrop,
	}
}

func writeNAL(p *BoundedPacer, timestamp uint32, marker bool, payload []byte) error {
	_, err := p.Write(&rtp.Header{Timestamp: timestamp, SSRC: 7, Marker: marker}, payload, nil)
	return err
}

func TestPacerDropsUntilIDRAndRecoversAfterOverflow(t *testing.T) {
	var drops atomic.Int32
	p := makeTestPacer(func(string) { drops.Add(1) })
	defer p.Close()

	if err := writeNAL(p, 1, true, []byte{0x41, 0x01}); err != nil {
		t.Fatal(err)
	}
	if frames := len(p.queue); frames != 0 {
		t.Fatalf("queued a dependent frame before the first IDR: %d", frames)
	}
	if err := writeNAL(p, 2, true, []byte{0x65, 0x01}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxQueuedFrames-1; i++ {
		if err := writeNAL(p, uint32(3+i), true, []byte{0x41, byte(i + 2)}); err != nil {
			t.Fatal(err)
		}
	}
	if frames := len(p.queue); frames != maxQueuedFrames {
		t.Fatalf("queue cap before overflow = %d, want %d", frames, maxQueuedFrames)
	}
	if err := writeNAL(p, 3+maxQueuedFrames-1, true, []byte{0x41, 0xfe}); err != nil {
		t.Fatal(err)
	}
	if frames, bytes := len(p.queue), p.queueBytes; frames != 0 || bytes != 0 {
		t.Fatalf("overflow did not clear queued reference frames: frames=%d bytes=%d", frames, bytes)
	}
	if !p.waitingIDR || drops.Load() != 1 {
		t.Fatalf("overflow recovery state: waitingIDR=%v drops=%d", p.waitingIDR, drops.Load())
	}
	if err := writeNAL(p, 4+maxQueuedFrames-1, true, []byte{0x41, 0x04}); err != nil {
		t.Fatal(err)
	}
	if frames := len(p.queue); frames != 0 {
		t.Fatalf("queued a non-IDR during recovery: %d", frames)
	}
	if err := writeNAL(p, 5+maxQueuedFrames-1, true, []byte{0x65, 0x05}); err != nil {
		t.Fatal(err)
	}
	if frames := len(p.queue); frames != 1 || p.waitingIDR {
		t.Fatalf("IDR did not restore the chain: frames=%d waitingIDR=%v", frames, p.waitingIDR)
	}
}

func TestPacerLimitsQueuedUnitsAroundInflightIDR(t *testing.T) {
	p := makeTestPacer(nil)
	defer p.Close()
	if err := writeNAL(p, 1, true, []byte{0x65, 0x01}); err != nil {
		t.Fatal(err)
	}
	unit := p.queue[0]
	p.queue = nil
	p.queueBytes = 0
	p.inFlight = true
	p.inFlightKeyframe = true
	p.inFlightBytes = unit.bytes

	for i := 0; i < maxQueuedFrames-1; i++ {
		if err := writeNAL(p, uint32(i+2), true, []byte{0x41, byte(i + 2)}); err != nil {
			t.Fatal(err)
		}
	}
	if frames := len(p.queue) + 1; frames != maxQueuedFrames {
		t.Fatalf("in-flight plus queue cap = %d, want %d", frames, maxQueuedFrames)
	}
	if err := writeNAL(p, maxQueuedFrames+1, true, []byte{0x41, 0xff}); err != nil {
		t.Fatal(err)
	}
	if !p.waitingIDR || len(p.queue) != 0 {
		t.Fatalf("overflow did not wait for recovery IDR: waiting=%v queued=%d", p.waitingIDR, len(p.queue))
	}
	if err := writeNAL(p, maxQueuedFrames+2, true, []byte{0x65, 0x04}); err != nil {
		t.Fatal(err)
	}
	if frames := len(p.queue) + 1; frames != 2 || p.waitingIDR {
		t.Fatalf("recovery unit exceeded queue cap: frames=%d waiting=%v", frames, p.waitingIDR)
	}
}

func TestPacerKeepsReferenceChainBehindSlowIDR(t *testing.T) {
	var drops atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	writer := &testRTPWriter{write: func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}}
	p := makeTestPacer(func(string) { drops.Add(1) })
	p.AddStream(7, writer)
	go p.run()
	defer p.Close()

	if err := writeNAL(p, 1, true, []byte{0x65, 0x01}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("IDR did not enter the pacer writer")
	}
	const dependentFrames = maxQueuedFrames - 1
	for i := 0; i < dependentFrames; i++ {
		if err := writeNAL(p, uint32(i+2), true, []byte{0x41, byte(i + 2)}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(maxQueueAge / 2)
	close(release)
	deadline := time.Now().Add(time.Second)
	for writer.writes.Load() < dependentFrames+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := writer.writes.Load(); got != dependentFrames+1 {
		t.Fatalf("writer sent %d packets, want the IDR and %d dependent frames", got, dependentFrames)
	}
	if drops.Load() != 0 || p.waitingIDR {
		t.Fatalf("slow IDR broke the reference chain: drops=%d waitingIDR=%v", drops.Load(), p.waitingIDR)
	}
}

func TestPacerDropsPartialAndOversizedUnits(t *testing.T) {
	dropReasons := make(chan string, 2)
	p := makeTestPacer(func(reason string) { dropReasons <- reason })
	defer p.Close()
	if err := writeNAL(p, 10, false, []byte{0x65, 0x01}); err != nil {
		t.Fatal(err)
	}
	if err := writeNAL(p, 11, true, []byte{0x41, 0x02}); err != nil {
		t.Fatal(err)
	}
	if reason := <-dropReasons; reason != "上一访问单元未收齐" || !p.waitingIDR {
		t.Fatalf("partial access unit recovery: reason=%q waiting=%v", reason, p.waitingIDR)
	}
	if err := writeNAL(p, 12, true, []byte{0x65, 0x03}); err != nil {
		t.Fatal(err)
	}
	if frames := len(p.queue); frames != 1 {
		t.Fatalf("valid IDR was not queued after partial-frame recovery: %d", frames)
	}
	if err := writeNAL(p, 13, true, append([]byte{0x65}, make([]byte, maxQueueBytes)...)); err != nil {
		t.Fatal(err)
	}
	if frames, bytes := len(p.queue), p.queueBytes; frames != 0 || bytes != 0 || !p.waitingIDR {
		t.Fatalf("oversized unit exceeded bounds: frames=%d bytes=%d waiting=%v", frames, bytes, p.waitingIDR)
	}
	if reason := <-dropReasons; reason != "帧超过队列字节上限" {
		t.Fatalf("oversized access unit recovery reason = %q", reason)
	}
}

func TestPacerExpiresAccessUnitsAfterQueueAge(t *testing.T) {
	dropped := make(chan string, 1)
	writer := &testRTPWriter{}
	p := &BoundedPacer{
		writers: map[uint32]interceptor.RTPWriter{7: writer},
		queue: []*accessUnit{{
			packets: []packet{{header: rtp.Header{Timestamp: 1, SSRC: 7, Marker: true}, payload: []byte{0x65, 0x01}}},
			ssrc:    7, created: time.Now().Add(-maxQueueAge - time.Millisecond), bytes: 14, keyframe: true,
		}},
		queueBytes: 14, targetBPS: 2_000_000, waitingIDR: false,
		done: make(chan struct{}), wake: make(chan struct{}, 1),
		onDrop: func(reason string) { dropped <- reason },
	}
	p.wake <- struct{}{}
	go p.run()
	defer p.Close()
	select {
	case reason := <-dropped:
		if reason != "出队访问单元过期" {
			t.Fatalf("expired access unit recovery reason = %q", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("expired access unit was not dropped")
	}
	if writer.writes.Load() != 0 || !p.waitingIDR {
		t.Fatalf("expired frame reached writer or skipped recovery: writes=%d waiting=%v", writer.writes.Load(), p.waitingIDR)
	}
	if frames, bytes := len(p.queue), p.queueBytes; frames != 0 || bytes != 0 {
		t.Fatalf("expired queue not cleared: frames=%d bytes=%d", frames, bytes)
	}
}

func TestPacketHasIDRRecognizesSingleSTAPAndFUAPackets(t *testing.T) {
	for name, payload := range map[string][]byte{
		"single NAL": {0x65, 0x01},
		"STAP-A":     {24, 0, 2, 0x67, 0x01, 0, 2, 0x65, 0x01},
		"FU-A":       {28, 0x85, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			if !packetHasIDR(payload) {
				t.Fatalf("packetHasIDR(%v) = false", payload)
			}
		})
	}
	if packetHasIDR([]byte{28, 0x05, 0x01}) || packetHasIDR([]byte{24, 0, 9, 0x65}) {
		t.Fatal("recognized a non-start or malformed fragmented packet as IDR")
	}
}

func TestPacingUsesPartialBatchBudget(t *testing.T) {
	p := NewBoundedPacer(2_000_000, nil)
	defer p.Close()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	p.AddStream(1, &testRTPWriter{})
	if _, ok := p.waitForSendBudget(1, &rtp.Header{}, 1200, 2_000_000, 0, timer); !ok {
		t.Fatal("first packet reservation failed")
	}
	if _, ok := p.waitForSendBudget(1, &rtp.Header{}, 1200, 2_000_000, 0, timer); !ok {
		t.Fatal("packet reservation failed")
	}
	p.mu.Lock()
	debt := p.pacingTokens
	p.mu.Unlock()
	if debt >= 0 {
		t.Fatal("partial packet budget was discarded instead of carried as debt")
	}
}

func TestNewIDRReplacesQueuedReferenceChain(t *testing.T) {
	var drops atomic.Int32
	p := makeTestPacer(func(string) { drops.Add(1) })
	defer p.Close()
	for i, nal := range []byte{0x65, 0x41, 0x65} {
		_, err := p.Write(&rtp.Header{SSRC: 1, Timestamp: uint32(i+1) * 1500, Marker: true}, []byte{nal, 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(p.queue) != 1 || !p.queue[0].keyframe || p.waitingIDR || drops.Load() != 0 {
		t.Fatal("new IDR did not replace old references without recovery")
	}
}
