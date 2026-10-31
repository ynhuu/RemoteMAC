package media

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

const (
	// Bound stale frames and queue memory without rejecting frames based on their bitrate estimate.
	maxQueuedFrames = 4
	maxQueueAge     = 100 * time.Millisecond
	// 3840x2160 8-bit 4:2:0 video is about 11.9 MiB uncompressed. Leave headroom
	// for a large encoded access unit and its RTP packet copies.
	maxQueueBytes    = 16 << 20
	mediaPacingBurst = time.Millisecond
	mediaPacingRate  = 60_000_000
)

var errPacerClosed = errors.New("RemoteMAC RTP pacer is closed")

type packet struct {
	header     rtp.Header
	payload    []byte
	attributes interceptor.Attributes
}

type accessUnit struct {
	packets  []packet
	ssrc     uint32
	created  time.Time
	bytes    int
	keyframe bool
}

// BoundedPacer queues complete H.264 access units, then forwards each unit's
// RTP packets consecutively. Dropping a reference unit invalidates the chain,
// so it waits for IDR.
type BoundedPacer struct {
	mu               sync.Mutex
	writers          map[uint32]interceptor.RTPWriter
	queue            []*accessUnit
	building         []packet
	buildingTS       uint32
	buildingSSRC     uint32
	buildingAt       time.Time
	buildingSize     int
	buildingIDR      bool
	buildingDrop     bool
	queueBytes       int
	pacingTokens     float64
	pacingUpdatedAt  time.Time
	inFlightBytes    int
	targetBPS        int
	waitingIDR       bool
	inFlight         bool
	inFlightKeyframe bool
	epoch            uint64
	done             chan struct{}
	wake             chan struct{}
	closed           bool
	onDrop           func(string)
	closeOnce        sync.Once
}

func NewBoundedPacer(initialBitrate int, onDrop func(string)) *BoundedPacer {
	p := &BoundedPacer{
		writers: make(map[uint32]interceptor.RTPWriter), targetBPS: initialBitrate,
		waitingIDR: true, done: make(chan struct{}), wake: make(chan struct{}, 1), onDrop: onDrop,
	}
	go p.run()
	return p
}

func (p *BoundedPacer) AddStream(ssrc uint32, writer interceptor.RTPWriter) {
	p.mu.Lock()
	p.writers[ssrc] = writer
	p.mu.Unlock()
}

func (p *BoundedPacer) RemoveStream(ssrc uint32) {
	p.mu.Lock()
	delete(p.writers, ssrc)
	p.mu.Unlock()
}

func (p *BoundedPacer) SetTargetBitrate(rate int) {
	if rate < 100_000 {
		rate = 100_000
	}
	p.mu.Lock()
	p.targetBPS = rate
	p.mu.Unlock()
}

// Reset invalidates in-flight packets and drops queued reference frames.
// The next video unit must be an IDR before any dependent frame is sent.
func (p *BoundedPacer) Reset() {
	p.mu.Lock()
	p.dropQueuedLocked()
	p.building = nil
	p.buildingSize = 0
	p.buildingIDR = false
	p.buildingDrop = false
	p.waitingIDR = true
	p.epoch++
	p.signalLocked()
	p.mu.Unlock()
}

func (p *BoundedPacer) signalLocked() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *BoundedPacer) Write(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
	if header == nil {
		return 0, nil
	}
	if header.Padding {
		return header.MarshalSize() + int(header.PaddingSize), nil
	}
	if len(payload) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, errPacerClosed
	}
	if p.buildingDrop {
		if header.Timestamp == p.buildingTS && header.SSRC == p.buildingSSRC {
			if header.Marker {
				p.buildingDrop = false
			}
			size := header.MarshalSize() + len(payload)
			p.mu.Unlock()
			return size, nil
		}
		p.buildingDrop = false
	}
	droppedPartial := false
	dropReason := ""
	if len(p.building) == 0 || header.Timestamp != p.buildingTS || header.SSRC != p.buildingSSRC {
		if len(p.building) > 0 {
			p.building = nil
			p.buildingSize = 0
			p.buildingIDR = false
			p.waitingIDR = true
			p.dropQueuedLocked()
			if !p.inFlightKeyframe {
				p.epoch++
				p.signalLocked()
			}
			droppedPartial = true
			dropReason = "上一访问单元未收齐"
		}
		p.building = p.building[:0]
		p.buildingTS = header.Timestamp
		p.buildingSSRC = header.SSRC
		p.buildingAt = time.Now()
		p.buildingSize = 0
		p.buildingIDR = false
	}
	packetSize := len(payload) + header.MarshalSize()
	if p.queueBytes+p.inFlightBytes+p.buildingSize+packetSize > maxQueueBytes {
		p.building = nil
		p.buildingSize = 0
		p.buildingIDR = false
		p.buildingDrop = !header.Marker
		p.buildingTS = header.Timestamp
		p.buildingSSRC = header.SSRC
		p.waitingIDR = true
		p.dropQueuedLocked()
		if !p.inFlightKeyframe {
			p.epoch++
			p.signalLocked()
		}
		callback := p.onDrop
		size := packetSize
		p.mu.Unlock()
		if callback != nil {
			callback("帧超过队列字节上限")
		}
		return size, nil
	}
	clonedHeader := header.Clone()
	clonedPayload := append([]byte(nil), payload...)
	var clonedAttributes interceptor.Attributes
	if len(attributes) > 0 {
		clonedAttributes = make(interceptor.Attributes, len(attributes))
		for key, value := range attributes {
			clonedAttributes[key] = value
		}
	}
	p.building = append(p.building, packet{header: clonedHeader, payload: clonedPayload, attributes: clonedAttributes})
	p.buildingSize += packetSize
	p.buildingIDR = p.buildingIDR || packetHasIDR(payload)
	if !header.Marker {
		size := header.MarshalSize() + len(payload)
		callback := p.onDrop
		p.mu.Unlock()
		if droppedPartial && callback != nil {
			callback(dropReason)
		}
		return size, nil
	}

	unit := &accessUnit{
		packets: p.building, ssrc: p.buildingSSRC, created: p.buildingAt,
		bytes: p.buildingSize, keyframe: p.buildingIDR,
	}
	p.building = nil
	p.buildingSize = 0
	p.buildingIDR = false
	if p.waitingIDR && !unit.keyframe {
		size := header.MarshalSize() + len(payload)
		callback := p.onDrop
		p.mu.Unlock()
		if droppedPartial && callback != nil {
			callback(dropReason)
		}
		return size, nil
	}
	frameAge := time.Since(unit.created)
	if frameAge > maxQueueAge {
		p.dropQueuedLocked()
		p.waitingIDR = true
		if !p.inFlightKeyframe {
			p.epoch++
			p.signalLocked()
		}
		callback := p.onDrop
		size := header.MarshalSize() + len(payload)
		p.mu.Unlock()
		if callback != nil {
			callback(fmt.Sprintf("访问单元入队时已过期：帧龄=%s，访问单元=%dKB", frameAge.Round(time.Millisecond), unit.bytes/1024))
		}
		return size, nil
	}
	if unit.keyframe {
		// A complete IDR replaces queued older references without another
		// recovery request, even when the queue is at capacity. Finish the
		// packet currently in flight first.
		p.dropQueuedLocked()
		p.waitingIDR = false
		p.queue = append(p.queue, unit)
		p.queueBytes += unit.bytes
		p.signalLocked()
		size := header.MarshalSize() + len(payload)
		p.mu.Unlock()
		return size, nil
	}
	occupied := p.occupiedUnitsLocked()
	if occupied >= maxQueuedFrames {
		queueFrames, queueBytes, inFlight, target := len(p.queue), p.queueBytes, p.inFlight, p.targetBPS
		inFlightBytes, inFlightKeyframe := p.inFlightBytes, p.inFlightKeyframe
		oldestQueueAge := time.Duration(0)
		for _, queued := range p.queue {
			if age := time.Since(queued.created); age > oldestQueueAge {
				oldestQueueAge = age
			}
		}
		p.dropQueuedLocked()
		p.waitingIDR = true
		if !p.inFlightKeyframe {
			p.epoch++
			p.signalLocked()
		}
		dropReason := fmt.Sprintf("访问单元队列达到帧数上限：占用=%d/%d，排队=%d帧/%dKB，最老排队=%s，在途=%t/关键帧=%t/剩余=%dKB，新帧=%dKB/帧龄=%s，目标=%d bit/s",
			occupied, maxQueuedFrames, queueFrames, queueBytes/1024, oldestQueueAge.Round(time.Millisecond), inFlight,
			inFlightKeyframe, inFlightBytes/1024, unit.bytes/1024, frameAge.Round(time.Millisecond), target)
		callback := p.onDrop
		size := header.MarshalSize() + len(payload)
		p.mu.Unlock()
		if callback != nil {
			callback(dropReason)
		}
		return size, nil
	}
	p.queue = append(p.queue, unit)
	p.queueBytes += unit.bytes
	p.signalLocked()
	size := header.MarshalSize() + len(payload)
	callback := p.onDrop
	p.mu.Unlock()
	if droppedPartial && callback != nil {
		callback(dropReason)
	}
	return size, nil
}

func (p *BoundedPacer) occupiedUnitsLocked() int {
	count := len(p.queue)
	if p.inFlight {
		count++
	}
	return count
}

func (p *BoundedPacer) run() {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var nextSequence uint16
	sequenceReady := false
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		}
		for {
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			if p.inFlight || len(p.queue) == 0 {
				p.mu.Unlock()
				break
			}
			unit := p.queue[0]
			p.queue[0] = nil
			p.queue = p.queue[1:]
			p.queueBytes -= unit.bytes
			if age := time.Since(unit.created); age > maxQueueAge {
				log.Printf("RTP pacer 恢复：原因=出队帧过期，帧龄=%s，访问单元=%dKB，关键帧=%t，待发=%d帧/%dKB，目标=%d bit/s",
					age.Round(time.Millisecond), unit.bytes/1024, unit.keyframe, len(p.queue), p.queueBytes/1024, p.targetBPS)
				p.dropQueuedLocked()
				p.waitingIDR = true
				p.epoch++
				p.signalLocked()
				callback := p.onDrop
				p.mu.Unlock()
				if callback != nil {
					callback("出队访问单元过期")
				}
				continue
			}
			p.inFlight = true
			p.inFlightKeyframe = unit.keyframe
			p.inFlightBytes = unit.bytes
			// VideoToolbox controls the configured average bitrate. Let RTP burst
			// faster so frame-size variation does not fill the latency queue.
			rateBPS := max(p.targetBPS, mediaPacingRate)
			epoch := p.epoch
			p.mu.Unlock()

			for index := range unit.packets {
				item := &unit.packets[index]
				wireSize := len(item.payload) + item.header.MarshalSize()
				writer, ok := p.waitForSendBudget(unit.ssrc, &item.header, wireSize, rateBPS, epoch, timer)
				if !ok {
					break
				}
				if !sequenceReady {
					nextSequence, sequenceReady = item.header.SequenceNumber, true
				}
				item.header.SequenceNumber = nextSequence
				nextSequence++
				if _, err := writer.Write(&item.header, item.payload, item.attributes); err != nil {
					log.Printf("RTP pacer 写入失败：%v", err)
					p.mu.Lock()
					if p.epoch == epoch {
						p.dropQueuedLocked()
						p.waitingIDR = true
						p.epoch++
						p.signalLocked()
					}
					p.inFlight = false
					p.inFlightKeyframe = false
					p.inFlightBytes = 0
					callback := p.onDrop
					p.mu.Unlock()
					if callback != nil {
						callback("RTP 写入失败：" + err.Error())
					}
					break
				}
				p.mu.Lock()
				p.inFlightBytes = max(0, p.inFlightBytes-wireSize)
				p.mu.Unlock()
			}
			p.mu.Lock()
			p.inFlight = false
			p.inFlightKeyframe = false
			p.inFlightBytes = 0
			p.mu.Unlock()
		}
	}
}

// waitForSendBudget reserves packet pacing capacity.
func (p *BoundedPacer) waitForSendBudget(ssrc uint32, header *rtp.Header, packetBytes, rateBPS int, epoch uint64, timer *time.Timer) (interceptor.RTPWriter, bool) {
	if header == nil || packetBytes <= 0 {
		return nil, false
	}
	rate := max(100_000, rateBPS*11/10)
	burstBytes := max(float64(packetBytes), float64(rate)*mediaPacingBurst.Seconds()/8)
	for {
		p.mu.Lock()
		if p.closed || p.epoch != epoch {
			p.mu.Unlock()
			return nil, false
		}
		writer := p.writers[ssrc]
		if writer == nil {
			p.mu.Unlock()
			return nil, false
		}
		now := time.Now()
		if p.pacingUpdatedAt.IsZero() {
			p.pacingTokens = burstBytes
		} else {
			elapsed := now.Sub(p.pacingUpdatedAt).Seconds()
			p.pacingTokens += elapsed * float64(rate) / 8
			if p.pacingTokens > burstBytes {
				p.pacingTokens = burstBytes
			}
		}
		p.pacingUpdatedAt = now
		if p.pacingTokens > 0 {
			p.pacingTokens -= float64(packetBytes)
			p.mu.Unlock()
			return writer, true
		}
		waitSeconds := -p.pacingTokens * 8 / float64(rate)
		p.mu.Unlock()

		wait := time.Duration(waitSeconds * float64(time.Second))
		// Refill a short batch rather than waking once for every RTP packet.
		wait = max(wait, mediaPacingBurst)
		timer.Reset(wait)
		select {
		case <-p.done:
			timer.Stop()
			return nil, false
		case <-timer.C:
		}
	}
}

func (p *BoundedPacer) dropQueuedLocked() {
	p.queue = nil
	p.queueBytes = 0
}

func (p *BoundedPacer) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.dropQueuedLocked()
		close(p.done)
		p.mu.Unlock()
	})
	return nil
}

func packetHasIDR(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	switch payload[0] & 0x1f {
	case 5:
		return true
	case 24: // STAP-A
		for offset := 1; offset+2 <= len(payload); {
			length := int(payload[offset])<<8 | int(payload[offset+1])
			offset += 2
			if length == 0 || offset+length > len(payload) {
				return false
			}
			if payload[offset]&0x1f == 5 {
				return true
			}
			offset += length
		}
	case 28: // FU-A start fragment of an IDR slice
		return len(payload) >= 2 && payload[1]&0x80 != 0 && payload[1]&0x1f == 5
	}
	return false
}

var _ interface {
	AddStream(uint32, interceptor.RTPWriter)
	SetTargetBitrate(int)
	Close() error
	Write(*rtp.Header, []byte, interceptor.Attributes) (int, error)
} = (*BoundedPacer)(nil)
