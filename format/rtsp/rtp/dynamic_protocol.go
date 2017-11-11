package rtp

import (
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/format/rtsp/sdp"
)

type DynamicProtocol interface {
	av.CodecData

	ParsePacket(pkt *av.Packet, buf []byte, timestamp uint32, flags int) (uint32, int)
	ParseSDP(*sdp.Media) (av.CodecData, error)
}
