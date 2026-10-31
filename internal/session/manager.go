package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/transport/v5/stdnet"
	"github.com/pion/webrtc/v4"
	"remotemac/internal/media"
	"remotemac/internal/native"
	"remotemac/internal/netshape"
)

type Manager struct {
	mu                sync.Mutex
	handoffMu         sync.Mutex
	active            *PeerSession
	closing           bool
	testNetworkShaper *netshape.Shaper
}

type PeerSession struct {
	manager        *Manager
	configMu       sync.Mutex
	releasing      atomic.Bool
	releaseDone    chan struct{}
	owner          *SignalSocket
	testShaper     *netshape.Shaper
	pc             *webrtc.PeerConnection
	track          *webrtc.TrackLocalStaticRTP
	sender         *webrtc.RTPSender
	pacer          *media.BoundedPacer
	receiverLimits h264Limits
	frames         chan native.Frame
	pointerMoves   chan inputEvent
	done           chan struct{}
	close          sync.Once
	mediaFailure   sync.Once
	frameMu        sync.Mutex
	needIDR        bool

	width         atomic.Int32
	height        atomic.Int32
	displayWidth  atomic.Int32
	displayHeight atomic.Int32
	mapVersion    atomic.Int32
	resizing      atomic.Bool
	bitrate       atomic.Int64
	fps           atomic.Int32
	pliMu         sync.Mutex
	lastPLI       time.Time
}

const (
	minimumMediaBitrate = 2_000_000
	maximumMediaBitrate = 50_000_000
	maximumDisplayArea  = 25_000_000
	defaultMediaBitrate = 5_000_000
	defaultVideoFPS     = 30
)

func validDisplaySize(width, height int) bool {
	if width < 320 || height < 200 {
		return false
	}
	return width <= maximumDisplayArea/height
}

func NewManager(testShaper *netshape.Shaper) *Manager {
	return &Manager{testNetworkShaper: testShaper}
}

func (m *Manager) Handle(socket *SignalSocket, message SignalMessage) error {
	m.mu.Lock()
	active, closing := m.active, m.closing
	m.mu.Unlock()
	if closing {
		return errors.New("RemoteMAC 正在关闭")
	}
	switch message.Type {
	case "offer":
		return m.startAfterRelease(socket, message.SDP, message.Width, message.Height, message.Bitrate, message.FPS)
	case "candidate":
		if active == nil || active.owner != socket || active.pc == nil {
			return errors.New("WebRTC 会话尚未建立")
		}
		if message.Candidate == nil || *message.Candidate == "" {
			return nil
		}
		return active.pc.AddICECandidate(webrtc.ICECandidateInit{
			Candidate: *message.Candidate, SDPMid: message.SDPMid, SDPMLineIndex: message.SDPMLine,
		})
	case "resize":
		if active == nil || active.owner != socket {
			return errors.New("当前 WebSocket 未持有桌面控制权")
		}
		return active.resize(message.Width, message.Height)
	case "quality":
		if active == nil || active.owner != socket {
			return errors.New("当前 WebSocket 未持有桌面控制权")
		}
		return active.setUserQuality(message.Bitrate, message.FPS)
	case "close":
		if active != nil && active.owner != socket {
			return errors.New("当前 WebSocket 未持有桌面控制权")
		}
		m.release(active)
		return nil
	default:
		return fmt.Errorf("未知信令消息类型 %q", message.Type)
	}
}

const previousSessionCleanupWait = 5 * time.Second

// startAfterRelease makes a new offer take ownership of the single desktop
// session. It serializes competing offers, disconnects the current owner, and
// waits for native display cleanup before creating the replacement session.
func (m *Manager) startAfterRelease(socket *SignalSocket, offer string, width, height, bitrate, fps int) error {
	m.handoffMu.Lock()
	defer m.handoffMu.Unlock()

	timer := time.NewTimer(previousSessionCleanupWait)
	defer timer.Stop()
	for {
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return errors.New("RemoteMAC 正在关闭")
		}
		active := m.active
		if active == nil {
			m.mu.Unlock()
			return m.start(socket, offer, width, height, bitrate, fps)
		}
		releasing := active.releasing.Load()
		releaseDone := active.releaseDone
		owner := active.owner
		m.mu.Unlock()

		if !releasing {
			if owner != nil && owner != socket {
				owner.Kick(SignalMessage{
					Type:  "error",
					Error: "当前会话已被新的远程连接替换",
					Fatal: true,
				})
			}
			m.release(active)
		}
		if releaseDone == nil {
			return ErrSessionCleanupPending
		}
		select {
		case <-releaseDone:
			// releaseDone closes after the manager clears its active slot.
			continue
		case <-timer.C:
			return ErrSessionCleanupPending
		}
	}
}

func (m *Manager) start(socket *SignalSocket, offer string, width, height, bitrate, fps int) error {
	if offer == "" {
		return errors.New("WebRTC offer 为空")
	}
	if width == 0 && height == 0 {
		width, height = 1920, 1080
	}
	if !validDisplaySize(width, height) {
		return errors.New("初始虚拟桌面尺寸超出允许范围")
	}
	if bitrate <= 0 {
		bitrate = defaultMediaBitrate
	}
	bitrate = max(minimumMediaBitrate, min(bitrate, maximumMediaBitrate))
	switch fps {
	case 15, 30, 60:
	default:
		fps = defaultVideoFPS
	}
	width, height = even(width), even(height)
	if !validDisplaySize(width, height) {
		return errors.New("初始虚拟桌面尺寸超出允许范围")
	}
	receiverLimits, err := h264ReceiverLimits(offer)
	if err != nil {
		return fmt.Errorf("读取浏览器 H.264 解码能力：%w", err)
	}
	if !h264FrameWithinLimits(width, height, fps, receiverLimits) {
		return errors.New("请求画面超出浏览器 H.264 解码能力")
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return errors.New("RemoteMAC 正在关闭")
	}
	if m.active != nil {
		m.mu.Unlock()
		return errors.New("已有远程控制会话正在运行")
	}
	s := &PeerSession{
		manager: m, releaseDone: make(chan struct{}),
		owner: socket, frames: make(chan native.Frame, 2), pointerMoves: make(chan inputEvent, 1), done: make(chan struct{}), needIDR: true,
		testShaper: m.testNetworkShaper, receiverLimits: receiverLimits,
	}
	s.width.Store(int32(width))
	s.height.Store(int32(height))
	s.displayWidth.Store(int32(width))
	s.displayHeight.Store(int32(height))
	s.fps.Store(int32(fps))
	s.bitrate.Store(int64(bitrate))
	// Reserve transport headroom above the configured media bitrate.
	s.pacer = media.NewBoundedPacer(bitrate*5/4, s.onPacerDrop)
	m.active = s
	m.mu.Unlock()
	go s.pointerMoveLoop()
	rollback := true
	defer func() {
		if rollback {
			m.release(s)
		}
	}()
	if err := s.createPeer(offer); err != nil {
		return err
	}
	displayWidth, displayHeight := native.ActiveDisplaySize()
	if displayWidth <= 0 || displayHeight <= 0 {
		displayWidth, displayHeight = width, height
	}
	s.displayWidth.Store(int32(displayWidth))
	s.displayHeight.Store(int32(displayHeight))
	if err := socket.Send(SignalMessage{
		Type: "quality", State: "固定使用用户配置", Width: width, Height: height,
		DisplayWidth: displayWidth, DisplayHeight: displayHeight,
		EncodeWidth: width, EncodeHeight: height, FPS: fps, Bitrate: bitrate,
	}); err != nil {
		return fmt.Errorf("发送初始视频参数：%w", err)
	}
	rollback = false
	go s.sendLoop()
	return nil
}

func (s *PeerSession) createPeer(offer string) error {
	mediaEngine := &webrtc.MediaEngine{}
	codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90_000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"}, {Type: "nack", Parameter: "pli"},
			},
		}, PayloadType: 102,
	}
	if err := mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("注册 H.264 编码：%w", err)
	}
	rtxCodec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeRTX, ClockRate: 90_000, SDPFmtpLine: "apt=102",
		},
		PayloadType: 103,
	}
	if err := mediaEngine.RegisterCodec(rtxCodec, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("注册 H.264 RTX 重传：%w", err)
	}
	registry := &interceptor.Registry{}
	// Keep the NACK history bounded for low-latency loss recovery.
	// Retransmissions go straight to the transport instead of re-entering the
	// bounded access-unit queue as stale media.
	if err := webrtc.ConfigureNackWithOptions(mediaEngine, registry, nil, nack.ResponderSize(1024)); err != nil {
		return fmt.Errorf("配置 RTP NACK 恢复：%w", err)
	}
	// Keep configured-rate pacing and bounded queue recovery independent of GCC.
	registry.Add(media.NewPacerFactory(s.pacer))
	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return fmt.Errorf("配置 RTCP 报告：%w", err)
	}
	apiOptions := []func(*webrtc.API){webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(registry)}
	if s.testShaper != nil {
		baseNet, netErr := stdnet.NewNet()
		if netErr != nil {
			return fmt.Errorf("创建 UDP 测试网络：%w", netErr)
		}
		settingEngine := webrtc.SettingEngine{}
		settingEngine.SetNet(netshape.Wrap(baseNet, s.testShaper))
		apiOptions = append(apiOptions, webrtc.WithSettingEngine(settingEngine))
	}
	api := webrtc.NewAPI(apiOptions...)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("创建 PeerConnection：%w", err)
	}
	s.pc = pc
	track, err := webrtc.NewTrackLocalStaticRTP(codec.RTPCodecCapability, "desktop", "remotemac")
	if err != nil {
		return fmt.Errorf("创建 H.264 视频轨道：%w", err)
	}
	s.track = track
	sender, err := pc.AddTrack(track)
	if err != nil {
		return fmt.Errorf("添加 H.264 视频轨道：%w", err)
	}
	s.sender = sender
	go s.readRTCP()
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		jsonCandidate := candidate.ToJSON()
		_ = s.owner.Send(SignalMessage{Type: "candidate", Candidate: &jsonCandidate.Candidate, SDPMid: jsonCandidate.SDPMid, SDPMLine: jsonCandidate.SDPMLineIndex})
	})
	pc.OnDataChannel(s.onDataChannel)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		_ = s.owner.Send(SignalMessage{Type: "state", State: strings.ToLower(state.String())})
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			go s.manager.release(s)
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return fmt.Errorf("设置客户端 offer：%w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("创建 WebRTC answer：%w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("设置 WebRTC answer：%w", err)
	}
	if err := s.owner.Send(SignalMessage{Type: "answer", SDP: answer.SDP}); err != nil {
		return fmt.Errorf("发送 WebRTC answer：%w", err)
	}
	if err := native.StartWithDisplaySize(int(s.width.Load()), int(s.height.Load()), int(s.width.Load()), int(s.height.Load()), int(s.fps.Load()), int(s.bitrate.Load()), s.onFrame, func(err error) {
		_ = s.owner.Send(SignalMessage{Type: "error", Error: err.Error(), Fatal: true})
		go s.manager.release(s)
	}); err != nil {
		return fmt.Errorf("启动 macOS 桌面采集：%w", err)
	}
	return nil
}

func (s *PeerSession) onDataChannel(channel *webrtc.DataChannel) {
	channel.OnOpen(func() { log.Printf("DataChannel 已连接：%s", channel.Label()) })
	channel.OnClose(func() { native.ReleaseAll() })
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		if s.resizing.Load() {
			return
		}
		if len(message.Data) > 64<<10 {
			return
		}
		var event inputEvent
		if err := json.Unmarshal(message.Data, &event); err != nil {
			return
		}
		if event.Type == "releaseAll" {
			native.ReleaseAll()
			return
		}
		if !validInput(event, s.mapVersion.Load()) {
			return
		}
		switch event.Type {
		case "pointer":
			if event.Action == "move" {
				enqueueLatestPointerMove(s.pointerMoves, event)
				return
			}
			s.injectPointer(event)
		case "key":
			_ = native.InjectKey(event.KeyCode, event.Down)
		case "text":
			_ = native.InjectText(event.Text)
		case "missionControl":
			_ = native.InvokeMissionControl()
		}
	})
}

func (s *PeerSession) pointerMoveLoop() {
	for {
		select {
		case <-s.done:
			return
		case event := <-s.pointerMoves:
			if s.isClosed() {
				return
			}
			if s.resizing.Load() || !validInput(event, s.mapVersion.Load()) {
				continue
			}
			s.injectPointer(event)
		}
	}
}

func (s *PeerSession) onFrame(frame native.Frame) {
	if s.isClosed() {
		return
	}
	if frame.Width == 0 || frame.Height == 0 || len(frame.AnnexB) == 0 {
		return
	}
	if frame.Keyframe {
		sps, err := validateEncodedH264Keyframe(frame.AnnexB, int(frame.Width), int(frame.Height), int(frame.FPS))
		if err != nil {
			s.failMedia(err)
			return
		}
		if !h264MacroblocksWithinLimits(sps.frameMBs, int(frame.FPS), s.receiverLimits) {
			s.failMedia(fmt.Errorf("VideoToolbox SPS 超出浏览器 H.264 解码能力：宏块=%d，帧率=%d", sps.frameMBs, frame.FPS))
			return
		}
	}
	s.frameMu.Lock()
	defer s.frameMu.Unlock()
	if s.needIDR && !frame.Keyframe {
		return
	}
	if len(s.frames) == cap(s.frames) {
		for len(s.frames) > 0 {
			<-s.frames
		}
		s.needIDR = true
		s.pacer.Reset()
		s.requestKeyframe()
		if !frame.Keyframe {
			_ = s.owner.Send(SignalMessage{Type: "state", State: "waiting-keyframe"})
			return
		}
	}
	if frame.Keyframe {
		s.needIDR = false
	}
	select {
	case s.frames <- frame:
	default:
		// The lock makes this unreachable unless the channel capacity changes.
		s.needIDR = true
		s.pacer.Reset()
		s.requestKeyframe()
	}
}

func (s *PeerSession) failMedia(err error) {
	s.mediaFailure.Do(func() {
		log.Printf("H.264 输出不符合协商解码能力：%v", err)
		if s.owner != nil {
			_ = s.owner.Send(SignalMessage{Type: "error", Error: "视频解码失败，正在重连", Fatal: true})
		}
		go s.manager.release(s)
	})
}

func (s *PeerSession) sendLoop() {
	packetizer := rtp.NewPacketizer(1200, 102, 0, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90_000)
	var lastCaptureAt, anchorCaptureAt time.Time
	var lastTimestamp, anchorTimestamp uint32
	var timestampMapVersion int32
	var usingCapturePTS bool
	for {
		select {
		case <-s.done:
			return
		case frame := <-s.frames:
			if time.Since(frame.EncodedAt) > 100*time.Millisecond {
				log.Printf("参考链恢复：原因=编码帧发送前过期，帧龄=%s", time.Since(frame.EncodedAt).Round(time.Millisecond))
				s.enterRecovery()
				continue
			}
			fps := max(1, int(frame.FPS))
			captureAt := frame.CaptureAt
			if captureAt.IsZero() {
				captureAt = frame.EncodedAt
			}
			duration := uint32(90_000 / fps)
			if !lastCaptureAt.IsZero() && !captureAt.IsZero() {
				delta := captureAt.Sub(lastCaptureAt)
				if delta > 0 && delta <= 10*time.Second {
					duration = uint32(uint64(delta) * 90_000 / uint64(time.Second))
					if duration == 0 {
						duration = 1
					}
				}
			}
			packets := packetizer.Packetize(frame.AnnexB, duration)
			if len(packets) == 0 {
				continue
			}
			timestamp := packets[0].Timestamp
			if !lastCaptureAt.IsZero() {
				timestamp = lastTimestamp + duration
				version := s.mapVersion.Load()
				sourceIsPTS := !frame.CaptureAt.IsZero()
				if version != timestampMapVersion || sourceIsPTS != usingCapturePTS || !captureAt.After(lastCaptureAt) {
					anchorCaptureAt, anchorTimestamp = captureAt, timestamp
					log.Printf("RTP 时钟重新锚定：映射=%d，使用采集PTS=%t", version, sourceIsPTS)
				} else {
					elapsed := captureAt.Sub(anchorCaptureAt)
					ticks := uint64(elapsed/time.Second)*90_000 + uint64(elapsed%time.Second)*90_000/uint64(time.Second)
					timestamp = anchorTimestamp + uint32(ticks)
				}
			} else {
				anchorCaptureAt, anchorTimestamp = captureAt, timestamp
			}
			timestampMapVersion, usingCapturePTS = s.mapVersion.Load(), !frame.CaptureAt.IsZero()
			lastCaptureAt, lastTimestamp = captureAt, timestamp
			for _, packet := range packets {
				packet.Timestamp = timestamp
				if s.isClosed() {
					return
				}
				if err := s.track.WriteRTP(packet); err != nil {
					log.Printf("发送 RTP：%v", err)
					s.enterRecovery()
					break
				}
			}
		}
	}
}

func (s *PeerSession) enterRecovery() {
	s.requireKeyframe()
	s.pacer.Reset()
	s.requestKeyframe()
}

func (s *PeerSession) onPacerDrop(reason string) {
	// The pacer already discarded its queued access units and is waiting for IDR.
	// Clear the upstream frame queue without resetting that newly established state.
	log.Printf("参考链恢复：原因=%s", reason)
	s.requireKeyframe()
	s.requestKeyframe()
}

func (s *PeerSession) requireKeyframe() {
	s.frameMu.Lock()
	if !s.needIDR {
		s.needIDR = true
		for len(s.frames) > 0 {
			<-s.frames
		}
	}
	s.frameMu.Unlock()
}

func (s *PeerSession) injectPointer(event inputEvent) {
	width, height := int(s.width.Load()), int(s.height.Load())
	x, y := scalePointer(event, width, height)
	_ = native.InjectPointer(event.Action, x, y, event.Button, width, height)
}

func (s *PeerSession) resize(width, height int) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.isClosed() {
		return errors.New("桌面会话已关闭")
	}
	if !validDisplaySize(width, height) {
		return errors.New("桌面尺寸超出允许范围")
	}
	width, height = even(width), even(height)
	if !validDisplaySize(width, height) {
		return errors.New("桌面尺寸超出允许范围")
	}
	if !h264FrameWithinLimits(width, height, int(s.fps.Load()), s.receiverLimits) {
		return errors.New("桌面尺寸超出浏览器 H.264 解码能力")
	}
	previousWidth, previousHeight := int(s.width.Load()), int(s.height.Load())
	if width == previousWidth && height == previousHeight {
		return nil
	}
	s.resizing.Store(true)
	defer s.resizing.Store(false)
	native.ReleaseAll()
	fps, bitrate := int(s.fps.Load()), int(s.bitrate.Load())
	if err := native.ResizeVirtualDisplay(width, height); err != nil {
		if errors.Is(err, native.ErrDisplayRecoveryFailed) {
			go s.manager.release(s)
			return fmt.Errorf("%w；正在关闭会话并恢复显示器", err)
		}
		return err
	}
	if err := native.SetVideoConfig(width, height, fps, bitrate); err != nil {
		displayErr := native.ResizeVirtualDisplay(previousWidth, previousHeight)
		var captureErr error
		if displayErr == nil {
			captureErr = native.SetVideoConfig(previousWidth, previousHeight, fps, bitrate)
		}
		if displayErr != nil || captureErr != nil {
			go s.manager.release(s)
			return fmt.Errorf("%w：应用桌面尺寸失败：%v；恢复旧会话失败（显示：%v，采集：%v）", ErrSessionRecoveryFailed, err, displayErr, captureErr)
		}
		if displayWidth, displayHeight := native.ActiveDisplaySize(); displayWidth > 0 && displayHeight > 0 {
			s.displayWidth.Store(int32(displayWidth))
			s.displayHeight.Store(int32(displayHeight))
		}
		return err
	}
	displayWidth, displayHeight := native.ActiveDisplaySize()
	if displayWidth <= 0 || displayHeight <= 0 {
		displayWidth, displayHeight = width, height
	}
	s.width.Store(int32(width))
	s.height.Store(int32(height))
	s.displayWidth.Store(int32(displayWidth))
	s.displayHeight.Store(int32(displayHeight))
	s.mapVersion.Add(1)
	s.enterRecovery()
	return s.owner.Send(SignalMessage{
		Type: "mapping", Width: width, Height: height,
		DisplayWidth: displayWidth, DisplayHeight: displayHeight,
		EncodeWidth: width, EncodeHeight: height,
		FPS: fps, MapVersion: int(s.mapVersion.Load()),
	})
}

func (s *PeerSession) setUserQuality(bitrate, fps int) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.isClosed() {
		return errors.New("桌面会话已关闭")
	}
	bitrate = max(minimumMediaBitrate, min(bitrate, maximumMediaBitrate))
	switch fps {
	case 15, 30, 60:
	default:
		fps = defaultVideoFPS
	}
	width, height := int(s.width.Load()), int(s.height.Load())
	if !h264FrameWithinLimits(width, height, fps, s.receiverLimits) {
		return errors.New("视频帧率超出浏览器 H.264 解码能力")
	}
	previousBitrate := int(s.bitrate.Load())
	reconfigured := int(s.fps.Load()) != fps
	if bitrate == previousBitrate && !reconfigured {
		return nil
	}
	if err := native.SetVideoConfig(width, height, fps, bitrate); err != nil {
		log.Printf("视频参数应用失败：%v", err)
		_ = s.owner.Send(SignalMessage{Type: "error", Error: "视频设置失败，正在重连", Fatal: true})
		go s.manager.release(s)
		return nil
	}
	s.bitrate.Store(int64(bitrate))
	s.fps.Store(int32(fps))
	s.pacer.SetTargetBitrate(bitrate * 5 / 4)
	log.Printf("固定视频参数已应用：码率=%d bit/s，尺寸=%dx%d@%d FPS", bitrate, width, height, fps)
	if reconfigured || bitrate < previousBitrate {
		// Discard frames encoded for the previous, higher target before pacing at
		// the lower rate. Otherwise they can linger long enough to become stale.
		s.enterRecovery()
	}
	return s.owner.Send(SignalMessage{
		Type: "quality", State: "固定使用用户配置",
		Width: int(s.width.Load()), Height: int(s.height.Load()),
		DisplayWidth: int(s.displayWidth.Load()), DisplayHeight: int(s.displayHeight.Load()),
		EncodeWidth: width, EncodeHeight: height, FPS: fps, Bitrate: bitrate,
	})
}

func (s *PeerSession) readRTCP() {
	buffer := make([]byte, 1500)
	for {
		n, _, err := s.sender.Read(buffer)
		if err != nil {
			return
		}
		packets, parseErr := rtcp.Unmarshal(buffer[:n])
		if parseErr != nil {
			continue
		}
		for _, packet := range packets {
			switch packet.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.requestKeyframe()
			}
		}
	}
}

func (s *PeerSession) requestKeyframe() {
	now := time.Now()
	s.pliMu.Lock()
	if !s.lastPLI.IsZero() && now.Sub(s.lastPLI) < 500*time.Millisecond {
		s.pliMu.Unlock()
		return
	}
	s.lastPLI = now
	s.pliMu.Unlock()
	_ = native.RequestKeyframe()
}

func (s *PeerSession) closeSession() {
	s.close.Do(func() {
		close(s.done)
		s.configMu.Lock()
		defer s.configMu.Unlock()
		native.ReleaseAll()
		if s.pc != nil {
			_ = s.pc.Close()
		}
		if s.pacer != nil {
			_ = s.pacer.Close()
		}
		native.Stop()
	})
}

func (s *PeerSession) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (m *Manager) release(s *PeerSession) {
	if s == nil {
		return
	}
	if !s.releasing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		s.closeSession()
		m.mu.Lock()
		if m.active == s {
			m.active = nil
		}
		m.mu.Unlock()
		if s.releaseDone != nil {
			close(s.releaseDone)
		}
	}()
}

func (m *Manager) CloseSocket(socket *SignalSocket) {
	m.mu.Lock()
	s := m.active
	m.mu.Unlock()
	if s != nil && s.owner == socket {
		m.release(s)
	}
}

func (m *Manager) CloseAll() error {
	m.mu.Lock()
	m.closing = true
	s := m.active
	m.mu.Unlock()
	if s != nil {
		m.release(s)
		if s.releaseDone != nil {
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-s.releaseDone:
			case <-timer.C:
				return errors.New("等待虚拟屏幕完成回收超时，服务继续退出")
			}
		}
	}
	return nil
}

func even(value int) int { return max(2, value&^1) }
