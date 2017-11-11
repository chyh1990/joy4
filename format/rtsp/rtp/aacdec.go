package rtp

import (
	"fmt"

	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/aacparser"
	"github.com/nareix/joy4/format/rtsp/sdp"
)

type AACDynamicProtocol struct {
}

/* Follows RFC 3640 */
func (h *AACDynamicProtocol) Type() av.CodecType {
	return av.AAC
}

func (h *AACDynamicProtocol) ParseSDP(sdp *sdp.Media) (av.CodecData, error) {
	if len(sdp.Config) == 0 {
		return nil, fmt.Errorf("rtp: aac sdp config missing")
	}
	// TODO: parse fmtp

	return aacparser.NewCodecDataFromMPEG4AudioConfigBytes(sdp.Config)
}

func (h *AACDynamicProtocol) ParsePacket(pkt *av.Packet, buf []byte, timestamp uint32, flags int) (uint32, int) {
	if len(buf) < 4 {
		fmt.Println("rtp: aac packet too short")
		return timestamp, -1
	}
	payload := buf[4:] // TODO: remove this hack
	pkt.Data = make([]byte, len(payload))
	copy(pkt.Data, payload)
	return timestamp, 0
}
