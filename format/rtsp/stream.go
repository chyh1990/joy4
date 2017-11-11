package rtsp

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/nareix/joy4/av"
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
		b[1] = byte(self.Idx*2 + idx)
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
				fmt.Println("rtp: drop udp packet")
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
	// self.udpConns = nil
	return nil
}

func (self *Stream) makeCodecData() (err error) {
	media := self.Sdp

	queueSize := 100
	if !self.client.UseUDP {
		queueSize = 0
	}
	self.ctx = rtp.NewRTPDemuxContext(media.PayloadType, queueSize)

	/*
		PT 	Encoding Name 	Audio/Video (A/V) 	Clock Rate (Hz) 	Channels 	Reference
		0	PCMU	A	8000	1	[RFC3551]
		1	Reserved
		2	Reserved
		3	GSM	A	8000	1	[RFC3551]
		4	G723	A	8000	1	[Vineet_Kumar][RFC3551]
		5	DVI4	A	8000	1	[RFC3551]
		6	DVI4	A	16000	1	[RFC3551]
		7	LPC	A	8000	1	[RFC3551]
		8	PCMA	A	8000	1	[RFC3551]
		9	G722	A	8000	1	[RFC3551]
		10	L16	A	44100	2	[RFC3551]
		11	L16	A	44100	1	[RFC3551]
		12	QCELP	A	8000	1	[RFC3551]
		13	CN	A	8000	1	[RFC3389]
		14	MPA	A	90000		[RFC3551][RFC2250]
		15	G728	A	8000	1	[RFC3551]
		16	DVI4	A	11025	1	[Joseph_Di_Pol]
		17	DVI4	A	22050	1	[Joseph_Di_Pol]
		18	G729	A	8000	1	[RFC3551]
		19	Reserved	A
		20	Unassigned	A
		21	Unassigned	A
		22	Unassigned	A
		23	Unassigned	A
		24	Unassigned	V
		25	CelB	V	90000		[RFC2029]
		26	JPEG	V	90000		[RFC2435]
		27	Unassigned	V
		28	nv	V	90000		[RFC3551]
		29	Unassigned	V
		30	Unassigned	V
		31	H261	V	90000		[RFC4587]
		32	MPV	V	90000		[RFC2250]
		33	MP2T	AV	90000		[RFC2250]
		34	H263	V	90000		[Chunrong_Zhu]
		35-71	Unassigned	?
		72-76	Reserved for RTCP conflict avoidance				[RFC3551]
		77-95	Unassigned	?
		96-127	dynamic	?			[RFC3551]
	*/
	switch {
	case media.PayloadType >= 96 && media.PayloadType <= 127:
		if !self.ctx.SetDynamicHandlerByCodecType(media.Type) {
			err = fmt.Errorf("rtp: unsupported codec type: %v", media.Type)
			return
		}
	/*
		case media.PayloadType == 0:
			self.CodecData = codec.NewPCMMulawCodecData()
		case media.PayloadType == 8:
			self.CodecData = codec.NewPCMAlawCodecData()
	*/
	default:
		if !self.ctx.SetDynamicHandlerByStaticId(int(media.PayloadType)) {
			err = fmt.Errorf("rtsp: PayloadType=%d unsupported", media.PayloadType)
			return
		}
	}

	var codecData av.CodecData
	if codecData, err = self.ctx.DynamicProtocol.ParseSDP(&media); err != nil {
		return
	}
	self.CodecData = codecData

	self.ctx.TimeScale = media.TimeScale
	// https://tools.ietf.org/html/rfc5391
	if self.ctx.TimeScale == 0 {
		self.ctx.TimeScale = 8000
	}

	return
}
