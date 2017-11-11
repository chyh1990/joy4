package rtsp

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec"
	"github.com/nareix/joy4/format/rtsp/rtp"
	"github.com/nareix/joy4/format/rtsp/sdp"
)

type Stream struct {
	av.CodecData
	Sdp    sdp.Media
	Idx    int
	client *Client

	ctx *rtp.RTPDemuxContext

	udpConns []*net.UDPConn
}

func (self *Stream) setupUDP() (err error) {
	self.udpConns = make([]*net.UDPConn, 2)
	udpAddr1, _ := net.ResolveUDPAddr("udp", ":0")
	if self.udpConns[0], err = net.ListenUDP("udp", udpAddr1); err != nil {
		return
	}
	udpAddr2, _ := net.ResolveUDPAddr("udp", ":0")
	if self.udpConns[1], err = net.ListenUDP("udp", udpAddr2); err != nil {
		self.udpConns[0].Close()
		return
	}
	go self.readUDP(0)
	go self.readUDP(1)
	if self.client.DebugRtsp {
		fmt.Println("rtsp: stream", self.Idx, ": rtp-rtcp: ",
			self.udpConns[0].LocalAddr(), self.udpConns[1].LocalAddr())
	}
	return nil
}

func (self *Stream) readUDP(idx int) {
	b := make([]byte, 65536+4)
	for {
		n, _, err := self.udpConns[idx].ReadFromUDP(b[4:])
		if err != nil {
			fmt.Println("rtp: ReadFromUDP: ", err)
			break
		}
		b[0] = '$'
		b[1] = byte(idx)
		binary.BigEndian.PutUint16(b[2:4], uint16(n))

		out := make([]byte, 4+n)
		copy(out, b)
		select {
		case self.client.udpCh <- udpPacket{
			Idx:  idx,
			Data: out,
		}:
		default:
			if self.client.DebugRtp {
				fmt.Println("drop udp packet")
			}
		}
	}
}

func (self *Stream) rtpUdpPort() int {
	return self.udpConns[0].LocalAddr().(*net.UDPAddr).Port
}

func (self *Stream) rtcpUdpPort() int {
	return self.udpConns[1].LocalAddr().(*net.UDPAddr).Port
}

func (self *Stream) Close() error {
	if self == nil {
		return nil
	}
	for _, udp := range self.udpConns {
		if udp != nil {
			udp.Close()
		}
	}
	self.udpConns = nil
	return nil
}

func (self *Stream) makeCodecData() (err error) {
	media := self.Sdp

	queueSize := 100
	if !self.client.UseUDP {
		queueSize = 0
	}
	self.ctx = rtp.NewRTPDemuxContext(media.PayloadType, queueSize)
	switch {
	case media.PayloadType >= 96 && media.PayloadType <= 127:
		if !self.ctx.SetDynamicHandlerByCodecType(media.Type) {
			err = fmt.Errorf("rtp: unsupported codec type: %v", media.Type)
			return
		}
		var codecData av.CodecData
		if codecData, err = self.ctx.DynamicProtocol.ParseSDP(&media); err != nil {
			return
		}
		self.CodecData = codecData
	case media.PayloadType == 0:
		self.CodecData = codec.NewPCMMulawCodecData()
	case media.PayloadType == 8:
		self.CodecData = codec.NewPCMAlawCodecData()
	default:
		err = fmt.Errorf("rtsp: PayloadType=%d unsupported", media.PayloadType)
		return
	}
	self.ctx.TimeScale = media.TimeScale
	// https://tools.ietf.org/html/rfc5391
	if self.ctx.TimeScale == 0 {
		self.ctx.TimeScale = 8000
	}

	return
}
