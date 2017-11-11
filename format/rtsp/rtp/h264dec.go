package rtp

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/h264parser"
	"github.com/nareix/joy4/format/rtsp/sdp"
)

type H264DynamicProtocol struct {
	ProfileIdc        uint8
	ProfileIop        uint8
	LevelIdc          uint8
	PacketizationMode int

	sps []byte
	pps []byte
}

var startSequence = []byte{0, 0, 0, 1}

func (h *H264DynamicProtocol) Type() av.CodecType {
	return av.H264
}

const NAL_MASK = 0x1f

func (h *H264DynamicProtocol) parseFragPacket(pkt *av.Packet, buf []byte, startBit byte, nalHeader []byte) {
	totLen := len(buf)
	pos := 0
	if startBit != 0 {
		totLen += 4 + len(nalHeader)
	}
	pkt.Data = make([]byte, totLen)
	if startBit != 0 {
		copy(pkt.Data, startSequence)
		pos += len(startSequence)
		copy(pkt.Data[pos:], nalHeader)
		pos += len(nalHeader)
	}
	copy(pkt.Data[pos:], buf)
}

func (h *H264DynamicProtocol) parseFUAPacket(pkt *av.Packet, buf []byte, nalMask int) int {
	/*
		0                   1                   2                   3
		0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
		+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		| FU indicator  |   FU header   |                               |
		+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+                               |
		|                                                               |
		|                         FU payload                            |
		|                                                               |
		|                               +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		|                               :...OPTIONAL RTP padding        |
		+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		Figure 14.  RTP payload format for FU-A

		The FU indicator octet has the following format:
		+---------------+
		|0|1|2|3|4|5|6|7|
		+-+-+-+-+-+-+-+-+
		|F|NRI|  Type   |
		+---------------+


		The FU header has the following format:
		+---------------+
		|0|1|2|3|4|5|6|7|
		+-+-+-+-+-+-+-+-+
		|S|E|R|  Type   |
		+---------------+

		S: 1 bit
		When set to one, the Start bit indicates the start of a fragmented
		NAL unit.  When the following FU payload is not the start of a
		fragmented NAL unit payload, the Start bit is set to zero.

		E: 1 bit
		When set to one, the End bit indicates the end of a fragmented NAL
		unit, i.e., the last byte of the payload is also the last byte of
		the fragmented NAL unit.  When the following FU payload is not the
		last fragment of a fragmented NAL unit, the End bit is set to
		zero.

		R: 1 bit
		The Reserved bit MUST be equal to 0 and MUST be ignored by the
		receiver.

		Type: 5 bits
		The NAL unit payload type as defined in table 7-1 of [1].
	*/

	if len(buf) < 3 {
		fmt.Println("Too short data for FU-A H.264 RTP packet")
		return -1
	}

	fuIndicator := buf[0]
	fuHeader := buf[1]
	startBit := fuHeader >> 7
	naltype := fuHeader & 0x1f
	nal := fuIndicator&0xe0 | naltype

	// fmt.Println("nal", naltype)
	pkt.IsKeyFrame = naltype == h264parser.NALU_IDR_SLICE

	h.parseFragPacket(pkt, buf[2:], startBit, []byte{nal})
	return 0
}

// return 0 on packet, no more left, 1 on packet, 1 on partial packet
func (h *H264DynamicProtocol) ParsePacket(pkt *av.Packet, buf []byte, timestamp uint32, flags int) (uint32, int) {
	rv := 0
	if len(buf) == 0 {
		return timestamp, -1
	}
	nal := buf[0]
	naltype := nal & 0x1f

	/*
		Table 7-1 – NAL unit type codes
		1   ￼Coded slice of a non-IDR picture
		5    Coded slice of an IDR picture
		6    Supplemental enhancement information (SEI)
		7    Sequence parameter set
		8    Picture parameter set
		1-23     NAL unit  Single NAL unit packet             5.6
		24       STAP-A    Single-time aggregation packet     5.7.1
		25       STAP-B    Single-time aggregation packet     5.7.1
		26       MTAP16    Multi-time aggregation packet      5.7.2
		27       MTAP24    Multi-time aggregation packet      5.7.2
		28       FU-A      Fragmentation unit                 5.8
		29       FU-B      Fragmentation unit                 5.8
		30-31    reserved                                     -
	*/

	switch {
	case naltype == 0: // undefined, but pass them through
		fallthrough
	case naltype >= 9 && naltype <= 23:
		fallthrough
	case naltype >= 1 && naltype <= 8:
		// fmt.Println("nal", naltype)
		// isSEI := naltype == h264parser.NALU_SEI
		isKeyFrame := naltype == h264parser.NALU_IDR_SLICE
		// avcc mode
		pkt.Data = make([]byte, 4+len(buf))
		copy(pkt.Data, startSequence)
		copy(pkt.Data[4:], buf)
		if isKeyFrame {
			pkt.IsKeyFrame = true
		}
	case naltype == 28: // FU-A (fragmented nal)
		rv = h.parseFUAPacket(pkt, buf, NAL_MASK)
	default:
		return timestamp, -1
	}

	return timestamp, rv
}

func (h *H264DynamicProtocol) handleSPSPPS(buf []byte) {
	naluType := buf[0] & 0x1f
	if naluType == h264parser.NALU_SPS {
		h.sps = make([]byte, len(buf))
		copy(h.sps, buf)
	} else if naluType == h264parser.NALU_PPS {
		h.pps = make([]byte, len(buf))
		copy(h.pps, buf)
	}
}

func (h *H264DynamicProtocol) ParseSDP(sdp *sdp.Media) (av.CodecData, error) {
	var sprop [][]byte
	for k, v := range sdp.ALines {
		if k == "sprop-parameter-sets" {
			fields := strings.Split(v, ",")
			for _, field := range fields {
				val, _ := base64.StdEncoding.DecodeString(field)
				sprop = append(sprop, val)

			}
		}
	}
	for _, nalu := range sprop {
		if len(nalu) > 0 {
			h.handleSPSPPS(nalu)
		}
	}

	if len(h.sps) == 0 || len(h.pps) == 0 {
		if nalus, typ := h264parser.SplitNALUs(sdp.Config); typ != h264parser.NALU_RAW {
			for _, nalu := range nalus {
				if len(nalu) > 0 {
					h.handleSPSPPS(nalu)
				}
			}
		}
	}

	if len(h.sps) > 0 && len(h.pps) > 0 {
		return h264parser.NewCodecDataFromSPSAndPPS(h.sps, h.pps)
	} else {
		return nil, fmt.Errorf("rtsp: missing h264 sps or pps")
	}
}
