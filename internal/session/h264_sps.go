package session

import (
	"errors"
	"fmt"
)

type h264SPS struct {
	profileIDC byte
	profileIOP byte
	levelIDC   byte
	width      int
	height     int
	frameMBs   int
}

type h264BitReader struct {
	data []byte
	bit  int
}

func (r *h264BitReader) readBit() (uint32, error) {
	if r.bit >= len(r.data)*8 {
		return 0, errors.New("SPS 数据不完整")
	}
	value := (r.data[r.bit/8] >> (7 - uint(r.bit%8))) & 1
	r.bit++
	return uint32(value), nil
}

func (r *h264BitReader) readBits(count int) (uint32, error) {
	if count < 0 || count > 32 || count > len(r.data)*8-r.bit {
		return 0, errors.New("SPS 位字段不完整")
	}
	var value uint32
	for range count {
		bit, err := r.readBit()
		if err != nil {
			return 0, err
		}
		value = value<<1 | bit
	}
	return value, nil
}

func (r *h264BitReader) readUE() (uint32, error) {
	leadingZeroBits := 0
	for {
		bit, err := r.readBit()
		if err != nil {
			return 0, err
		}
		if bit != 0 {
			break
		}
		leadingZeroBits++
		if leadingZeroBits > 31 {
			return 0, errors.New("SPS 无符号指数哥伦布值溢出")
		}
	}
	suffix, err := r.readBits(leadingZeroBits)
	if err != nil {
		return 0, err
	}
	return ((uint32(1) << leadingZeroBits) - 1) + suffix, nil
}

func (r *h264BitReader) readSE() (int32, error) {
	codeNum, err := r.readUE()
	if err != nil {
		return 0, err
	}
	if codeNum&1 == 0 {
		return -int32(codeNum / 2), nil
	}
	return int32((codeNum + 1) / 2), nil
}

func parseH264SPSNAL(nal []byte) (h264SPS, error) {
	if len(nal) < 5 || nal[0]&0x80 != 0 || nal[0]&0x1f != 7 {
		return h264SPS{}, errors.New("缺少有效的 H.264 SPS NAL 单元")
	}
	rbsp := h264RBSP(nal[1:])
	if len(rbsp) < 4 {
		return h264SPS{}, errors.New("H.264 SPS 头部不完整")
	}
	sps := h264SPS{profileIDC: rbsp[0], profileIOP: rbsp[1], levelIDC: rbsp[2]}
	if !isConstrainedBaselineProfile(sps.profileIDC, sps.profileIOP) {
		return h264SPS{}, fmt.Errorf("VideoToolbox 输出了非 Constrained Baseline SPS（%02x%02x%02x）", sps.profileIDC, sps.profileIOP, sps.levelIDC)
	}
	if h264LevelCode(sps.profileIDC, sps.profileIOP, sps.levelIDC) == 0 {
		return h264SPS{}, fmt.Errorf("VideoToolbox 输出了未知 H.264 level_idc=%d", sps.levelIDC)
	}

	reader := h264BitReader{data: rbsp[3:]}
	spsID, err := reader.readUE()
	if err != nil || spsID > 31 {
		return h264SPS{}, errors.New("H.264 SPS 序列参数集编号无效")
	}
	log2MaxFrameNum, err := reader.readUE()
	if err != nil || log2MaxFrameNum > 12 {
		return h264SPS{}, errors.New("H.264 SPS 帧编号字段无效")
	}
	picOrderCountType, err := reader.readUE()
	if err != nil || picOrderCountType > 2 {
		return h264SPS{}, errors.New("H.264 SPS 图像顺序字段无效")
	}
	switch picOrderCountType {
	case 0:
		log2MaxPicOrderCount, readErr := reader.readUE()
		if readErr != nil || log2MaxPicOrderCount > 12 {
			return h264SPS{}, errors.New("H.264 SPS POC 字段无效")
		}
	case 1:
		if _, err = reader.readBit(); err != nil {
			return h264SPS{}, err
		}
		if _, err = reader.readSE(); err != nil {
			return h264SPS{}, err
		}
		if _, err = reader.readSE(); err != nil {
			return h264SPS{}, err
		}
		cycleLength, readErr := reader.readUE()
		if readErr != nil || cycleLength > 255 {
			return h264SPS{}, errors.New("H.264 SPS POC 周期长度无效")
		}
		for range cycleLength {
			if _, err = reader.readSE(); err != nil {
				return h264SPS{}, err
			}
		}
	}
	if _, err = reader.readUE(); err != nil { // max_num_ref_frames
		return h264SPS{}, err
	}
	if _, err = reader.readBit(); err != nil { // gaps_in_frame_num_value_allowed_flag
		return h264SPS{}, err
	}
	widthInMBsMinus1, err := reader.readUE()
	if err != nil || widthInMBsMinus1 >= 8192 {
		return h264SPS{}, errors.New("H.264 SPS 图像宽度字段无效")
	}
	heightInMapUnitsMinus1, err := reader.readUE()
	if err != nil || heightInMapUnitsMinus1 >= 8192 {
		return h264SPS{}, errors.New("H.264 SPS 图像高度字段无效")
	}
	frameMbsOnly, err := reader.readBit()
	if err != nil {
		return h264SPS{}, err
	}
	if frameMbsOnly == 0 {
		if _, err = reader.readBit(); err != nil { // mb_adaptive_frame_field_flag
			return h264SPS{}, err
		}
	}
	if _, err = reader.readBit(); err != nil { // direct_8x8_inference_flag
		return h264SPS{}, err
	}
	var cropLeft, cropRight, cropTop, cropBottom uint32
	cropping, err := reader.readBit()
	if err != nil {
		return h264SPS{}, err
	}
	if cropping != 0 {
		if cropLeft, err = reader.readUE(); err != nil {
			return h264SPS{}, err
		}
		if cropRight, err = reader.readUE(); err != nil {
			return h264SPS{}, err
		}
		if cropTop, err = reader.readUE(); err != nil {
			return h264SPS{}, err
		}
		if cropBottom, err = reader.readUE(); err != nil {
			return h264SPS{}, err
		}
	}

	frameHeightInMBs := (uint64(2) - uint64(frameMbsOnly)) * (uint64(heightInMapUnitsMinus1) + 1)
	widthInMBs := uint64(widthInMBsMinus1) + 1
	width := widthInMBs * 16
	height := frameHeightInMBs * 16
	// Constrained Baseline uses 4:2:0 chroma. Crop units are therefore two
	// pixels in each direction for progressive frames, and twice that vertically
	// for field-coded frames.
	cropUnitY := uint64(2) * (uint64(2) - uint64(frameMbsOnly))
	cropWidth := (uint64(cropLeft) + uint64(cropRight)) * 2
	cropHeight := (uint64(cropTop) + uint64(cropBottom)) * cropUnitY
	if cropWidth >= width || cropHeight >= height {
		return h264SPS{}, errors.New("H.264 SPS 裁剪范围超出图像尺寸")
	}
	sps.width = int(width - cropWidth)
	sps.height = int(height - cropHeight)
	sps.frameMBs = int(widthInMBs * frameHeightInMBs)
	return sps, nil
}

func h264RBSP(ebsp []byte) []byte {
	rbsp := make([]byte, 0, len(ebsp))
	zeroCount := 0
	for index := 0; index < len(ebsp); index++ {
		value := ebsp[index]
		if zeroCount >= 2 && value == 0x03 {
			zeroCount = 0
			continue
		}
		rbsp = append(rbsp, value)
		if value == 0 {
			zeroCount++
		} else {
			zeroCount = 0
		}
	}
	return rbsp
}

func validateEncodedH264Keyframe(annexB []byte, width, height, fps int) (h264SPS, error) {
	spsNAL, err := findH264SPS(annexB)
	if err != nil {
		return h264SPS{}, err
	}
	sps, err := parseH264SPSNAL(spsNAL)
	if err != nil {
		return h264SPS{}, err
	}
	level := h264LevelCode(sps.profileIDC, sps.profileIOP, sps.levelIDC)
	if level > localH264Level {
		return h264SPS{}, fmt.Errorf("VideoToolbox SPS level %d 超出本机 H.264 Level 5.2 编码上限", level)
	}
	if sps.width != width || sps.height != height {
		return h264SPS{}, fmt.Errorf("VideoToolbox SPS 尺寸为 %dx%d，与当前编码配置 %dx%d 不一致", sps.width, sps.height, width, height)
	}
	if sps.frameMBs > localH264MaxFS {
		return h264SPS{}, fmt.Errorf("VideoToolbox SPS 帧大小为 %d 个宏块，超过本机 H.264 Level 5.2 上限 %d", sps.frameMBs, localH264MaxFS)
	}
	if fps <= 0 || sps.frameMBs > localH264MaxMBPS/fps {
		return h264SPS{}, fmt.Errorf("VideoToolbox SPS 帧率配置 %d FPS 超过本机 H.264 Level 5.2 宏块速率上限 %d", fps, localH264MaxMBPS)
	}
	return sps, nil
}

func findH264SPS(annexB []byte) ([]byte, error) {
	start, prefixLength := nextH264StartCode(annexB, 0)
	for start >= 0 {
		nalStart := start + prefixLength
		nextStart, nextPrefixLength := nextH264StartCode(annexB, nalStart)
		nalEnd := len(annexB)
		if nextStart >= 0 {
			nalEnd = nextStart
		}
		if nalStart < nalEnd && annexB[nalStart]&0x1f == 7 {
			return annexB[nalStart:nalEnd], nil
		}
		start, prefixLength = nextStart, nextPrefixLength
	}
	return nil, errors.New("VideoToolbox IDR 访问单元没有携带 SPS")
}

func nextH264StartCode(data []byte, from int) (int, int) {
	for index := max(0, from); index+3 <= len(data); index++ {
		if data[index] != 0 || data[index+1] != 0 {
			continue
		}
		if index+4 <= len(data) && data[index+2] == 0 && data[index+3] == 1 {
			return index, 4
		}
		if data[index+2] == 1 {
			return index, 3
		}
	}
	return -1, 0
}
