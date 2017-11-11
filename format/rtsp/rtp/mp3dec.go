package rtp

import (
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/mp3parser"
	"github.com/nareix/joy4/format/rtsp/sdp"
)

type MP3DynamicProtocol struct {
}

func (h *MP3DynamicProtocol) Type() av.CodecType {
	return av.MP3
}

func (h *MP3DynamicProtocol) ParsePacket(pkt *av.Packet, buf []byte, timestamp uint32, flags int) (uint32, int) {
	if len(buf) < 4 {
		return timestamp, -1
	}
	// head := binary.BigEndian.Uint32(buf)
	buf = buf[4:]
	pkt.Data = make([]byte, len(buf))
	copy(pkt.Data, buf)
	return timestamp, 0
}

func (h *MP3DynamicProtocol) ParseSDP(sdp *sdp.Media) (av.CodecData, error) {
	// return mp3parser.NewCodecDataFromMP3AudioConfigBytes(sdp.Config)
	return mp3parser.CodecData{}, nil
}
