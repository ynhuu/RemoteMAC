package session

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	localH264Level   = 52
	localH264MaxFS   = 36_864
	localH264MaxMBPS = 2_073_600
)

var errNoCompatibleH264 = errors.New("offer 中没有支持 packetization-mode=1 的 H.264 Constrained Baseline")

type h264Limits struct {
	maxFS   int
	maxMBPS int
}

type h264CodecOffer struct {
	profileLevelID string
	parameters     map[string]string
}

// h264ReceiverLimits intersects the peer's advertised decoder limits with the
// local encoder's Constrained Baseline level 5.2 ceiling. max-fs and max-mbps
// may explicitly extend the receive capability beyond profile-level-id.
func h264ReceiverLimits(offer string) (h264Limits, error) {
	payloads := []string(nil)
	video := false
	codecs := make(map[string]string)
	formats := make(map[string]map[string]string)
	for _, raw := range strings.Split(strings.ReplaceAll(offer, "\r", ""), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "m=") {
			video = false
			fields := strings.Fields(line)
			if len(fields) >= 4 && strings.EqualFold(fields[0], "m=video") {
				video = true
				payloads = fields[3:]
			}
			continue
		}
		if !video {
			continue
		}
		if value, ok := strings.CutPrefix(line, "a=rtpmap:"); ok {
			parts := strings.SplitN(value, " ", 2)
			if len(parts) == 2 {
				codecs[parts[0]] = strings.ToLower(strings.TrimSpace(parts[1]))
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "a=fmtp:"); ok {
			parts := strings.SplitN(value, " ", 2)
			if len(parts) != 2 {
				continue
			}
			parameters := make(map[string]string)
			for _, parameter := range strings.Split(parts[1], ";") {
				key, value, hasValue := strings.Cut(strings.TrimSpace(parameter), "=")
				if hasValue {
					parameters[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
				}
			}
			formats[parts[0]] = parameters
		}
	}

	var selected *h264CodecOffer
	for _, payload := range payloads {
		if !strings.HasPrefix(codecs[payload], "h264/") {
			continue
		}
		parameters := formats[payload]
		if parameters["packetization-mode"] != "1" {
			continue
		}
		profileLevelID := strings.ToLower(parameters["profile-level-id"])
		if len(profileLevelID) != 6 || !strings.HasPrefix(profileLevelID, "42e0") {
			continue
		}
		selected = &h264CodecOffer{profileLevelID: profileLevelID, parameters: parameters}
		break
	}
	if selected == nil {
		return h264Limits{}, errNoCompatibleH264
	}

	profileLevel, err := hex.DecodeString(selected.profileLevelID)
	if err != nil || len(profileLevel) != 3 {
		return h264Limits{}, errNoCompatibleH264
	}
	remoteLevel := h264LevelCode(profileLevel[0], profileLevel[1], profileLevel[2])
	if remoteLevel == 0 {
		return h264Limits{}, fmt.Errorf("H.264 profile-level-id %q 包含未知级别", selected.profileLevelID)
	}
	if selected.parameters["level-asymmetry-allowed"] == "1" {
		if value := selected.parameters["max-recv-level"]; value != "" {
			maxReceiveLevel, decodeErr := hex.DecodeString(value)
			if decodeErr != nil || len(maxReceiveLevel) != 2 || maxReceiveLevel[0] != profileLevel[1] {
				return h264Limits{}, errors.New("H.264 max-recv-level 参数无效")
			}
			if level := h264LevelCode(profileLevel[0], maxReceiveLevel[0], maxReceiveLevel[1]); level == 0 {
				return h264Limits{}, errors.New("H.264 max-recv-level 指定了未知级别")
			} else if level > remoteLevel {
				remoteLevel = level
			}
		}
	}
	if remoteLevel > localH264Level {
		remoteLevel = localH264Level
	}
	limits, ok := h264LimitsForLevel(profileLevel[0], profileLevel[1], remoteLevel)
	if !ok {
		return h264Limits{}, fmt.Errorf("H.264 level %d 暂不支持", remoteLevel)
	}
	limits.maxFS = min(limits.maxFS, localH264MaxFS)
	limits.maxMBPS = min(limits.maxMBPS, localH264MaxMBPS)
	if value := selected.parameters["max-fs"]; value != "" {
		maxFS, parseErr := positiveSDPInteger("max-fs", value)
		if parseErr != nil {
			return h264Limits{}, parseErr
		}
		limits.maxFS = min(maxFS, localH264MaxFS)
	}
	if value := selected.parameters["max-mbps"]; value != "" {
		maxMBPS, parseErr := positiveSDPInteger("max-mbps", value)
		if parseErr != nil {
			return h264Limits{}, parseErr
		}
		limits.maxMBPS = min(maxMBPS, localH264MaxMBPS)
	}
	// The native bridge requires at least 320x200. Reject a decoder limit that
	// cannot represent that canvas even at one frame per second.
	minimumFrameMBs := macroblockCount(320, 200)
	if limits.maxFS < minimumFrameMBs || limits.maxMBPS < minimumFrameMBs {
		return h264Limits{}, errors.New("客户端 H.264 级别不足以解码 320x200 桌面画面")
	}
	limits.maxFS = min(limits.maxFS, limits.maxMBPS)
	return limits, nil
}

func h264FrameWithinLimits(width, height, fps int, limits h264Limits) bool {
	if width <= 0 || height <= 0 || fps <= 0 || limits.maxFS <= 0 || limits.maxMBPS <= 0 {
		return false
	}
	return h264MacroblocksWithinLimits(macroblockCount(width, height), fps, limits)
}

func h264MacroblocksWithinLimits(frameMBs, fps int, limits h264Limits) bool {
	if frameMBs <= 0 || fps <= 0 || limits.maxFS <= 0 || limits.maxMBPS <= 0 {
		return false
	}
	return frameMBs <= limits.maxFS && frameMBs <= limits.maxMBPS/fps
}

// RFC 6184 Table 5 identifies Constrained Baseline with profile_idc 66 and
// profile-iop bits x1xx0000. The variable bits are part of the same sub-profile.
func isConstrainedBaselineProfile(profileIDC, profileIOP byte) bool {
	return profileIDC == 0x42 && profileIOP&0x4f == 0x40
}

func positiveSDPInteger(name, value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("H.264 %s 参数无效", name)
	}
	return parsed, nil
}

func h264LevelCode(profileIDC, profileIOP, levelIDC byte) int {
	if profileIDC == 0x42 || profileIDC == 0x4d || profileIDC == 0x58 {
		if levelIDC == 11 && profileIOP&0x10 != 0 {
			return 9 // Level 1b for Baseline, Main, and Extended profiles.
		}
	}
	switch levelIDC {
	case 10, 11, 12, 13, 20, 21, 22, 30, 31, 32,
		40, 41, 42, 50, 51, 52, 60, 61, 62:
		return int(levelIDC)
	default:
		return 0
	}
}

func h264LimitsForLevel(profileIDC, profileIOP byte, level int) (h264Limits, bool) {
	if level > localH264Level {
		level = localH264Level
	}
	if !isConstrainedBaselineProfile(profileIDC, profileIOP) {
		return h264Limits{}, false
	}
	switch level {
	case 9, 10:
		return h264Limits{maxFS: 99, maxMBPS: 1485}, true
	case 11:
		return h264Limits{maxFS: 396, maxMBPS: 3000}, true
	case 12:
		return h264Limits{maxFS: 396, maxMBPS: 6000}, true
	case 13, 20:
		return h264Limits{maxFS: 396, maxMBPS: 11880}, true
	case 21:
		return h264Limits{maxFS: 792, maxMBPS: 19800}, true
	case 22:
		return h264Limits{maxFS: 1620, maxMBPS: 20250}, true
	case 30:
		return h264Limits{maxFS: 1620, maxMBPS: 40500}, true
	case 31:
		return h264Limits{maxFS: 3_600, maxMBPS: 108_000}, true
	case 32:
		return h264Limits{maxFS: 5_120, maxMBPS: 216_000}, true
	case 40, 41:
		return h264Limits{maxFS: 8_192, maxMBPS: 245_760}, true
	case 42:
		return h264Limits{maxFS: 8_704, maxMBPS: 522_240}, true
	case 50:
		return h264Limits{maxFS: 22_080, maxMBPS: 589_824}, true
	case 51:
		return h264Limits{maxFS: 36_864, maxMBPS: 983_040}, true
	case 52:
		return h264Limits{maxFS: localH264MaxFS, maxMBPS: localH264MaxMBPS}, true
	default:
		return h264Limits{}, false
	}
}

func macroblockCount(width, height int) int {
	return ((width + 15) / 16) * ((height + 15) / 16)
}
