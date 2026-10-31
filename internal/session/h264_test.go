package session

import (
	"strings"
	"testing"
)

const chromeH264Offer = `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 102
a=rtpmap:102 H264/90000
a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f`

func TestH264ReceiverLimitsFromChromeLevel31Offer(t *testing.T) {
	limits, err := h264ReceiverLimits(chromeH264Offer)
	if err != nil {
		t.Fatal(err)
	}
	if limits.maxFS != 3600 || limits.maxMBPS != 108_000 {
		t.Fatalf("level 3.1 limits = %+v, want MaxFS=3600 MaxMBPS=108000", limits)
	}
}

func TestH264ReceiverLimitsHonorMaxFSAndMaxMBPS(t *testing.T) {
	level21 := strings.Replace(chromeH264Offer, "42e01f", "42e015", 1)
	offer := level21 + ";max-fs=2500;max-mbps=75000"
	limits, err := h264ReceiverLimits(offer)
	if err != nil {
		t.Fatal(err)
	}
	if limits.maxFS != 2500 || limits.maxMBPS != 75_000 {
		t.Fatalf("explicit receiver limits = %+v, want MaxFS=2500 MaxMBPS=75000", limits)
	}
}

func TestH264ReceiverLimitsUseMaxReceiveLevelAndClampToLocalLevel(t *testing.T) {
	offer := strings.Replace(chromeH264Offer, "42e01f", "42e015;max-recv-level=e034", 1)
	limits, err := h264ReceiverLimits(offer)
	if err != nil {
		t.Fatal(err)
	}
	if limits.maxFS != localH264MaxFS || limits.maxMBPS != localH264MaxMBPS {
		t.Fatalf("advertised level above local Level 5.2 encoder limit was not capped: %+v", limits)
	}
}

func TestH264ReceiverLimitsAllowVerifiedCapabilitiesAboveProfileLevel(t *testing.T) {
	offer := chromeH264Offer + ";max-fs=14400;max-mbps=864000"
	limits, err := h264ReceiverLimits(offer)
	if err != nil {
		t.Fatal(err)
	}
	if limits.maxFS != 14_400 || limits.maxMBPS != 864_000 {
		t.Fatalf("extended receiver limits = %+v, want MaxFS=14400 MaxMBPS=864000", limits)
	}
}

func TestH264ReceiverLimitsHonorCapabilityBelowProfileLevel(t *testing.T) {
	offer := chromeH264Offer + ";max-fs=2500;max-mbps=75000"
	limits, err := h264ReceiverLimits(offer)
	if err != nil {
		t.Fatal(err)
	}
	if limits.maxFS != 2500 || limits.maxMBPS != 75_000 {
		t.Fatalf("lower explicit receiver limits = %+v, want MaxFS=2500 MaxMBPS=75000", limits)
	}
	tooLow := chromeH264Offer + ";max-fs=100;max-mbps=100"
	if _, err := h264ReceiverLimits(tooLow); err == nil {
		t.Fatal("accepted receiver limits below the minimum desktop canvas")
	}
}

func TestH264FrameWithinLimits(t *testing.T) {
	limits := h264Limits{maxFS: 8_160, maxMBPS: 244_800}
	if !h264FrameWithinLimits(1920, 1080, 30, limits) {
		t.Fatal("accepted 1920x1080 at 30 FPS was rejected")
	}
	if h264FrameWithinLimits(2560, 1440, 60, limits) {
		t.Fatal("frame exceeding receiver macroblock rate was accepted")
	}
	if !h264MacroblocksWithinLimits(8_160, 30, limits) {
		t.Fatal("valid encoded macroblock count was rejected")
	}
	if h264MacroblocksWithinLimits(8_161, 30, limits) {
		t.Fatal("encoded macroblock count exceeding max-fs was accepted")
	}
}

func TestH264ReceiverLimitsRespectLowerLevelAndRejectUnsupportedMinimum(t *testing.T) {
	offer := strings.Replace(chromeH264Offer, "42e01f", "42e015", 1)
	limits, err := h264ReceiverLimits(offer)
	if err != nil {
		t.Fatal(err)
	}
	if limits.maxFS != 792 || limits.maxMBPS != 19_800 {
		t.Fatalf("level 2.1 limits = %+v, want MaxFS=792 MaxMBPS=19800", limits)
	}

	level10 := strings.Replace(chromeH264Offer, "42e01f", "42e010", 1)
	if _, err := h264ReceiverLimits(level10); err == nil {
		t.Fatal("accepted a decoder level that cannot represent the minimum desktop canvas")
	}
}

func TestH264ReceiverLimitsRejectMissingOrMalformedCodecParameters(t *testing.T) {
	if _, err := h264ReceiverLimits("v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\n"); err == nil {
		t.Fatal("accepted an offer without H.264 codec parameters")
	}
	malformed := strings.Replace(chromeH264Offer, "42e01f", "42e0zz", 1)
	if _, err := h264ReceiverLimits(malformed); err == nil {
		t.Fatal("accepted a malformed profile-level-id")
	}
}
