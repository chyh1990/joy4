package rtp

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"

	"github.com/nareix/joy4/av"
)

type RTPStatistics struct {
	// highest sequence number seen
	MaxSeq uint16
	// shifted count of sequence number cycles
	Cycles uint32
	// base sequence number
	BaseSeq uint32
	// last bad sequence number + 1
	BadSeq uint32
	// sequence packets till source is valid
	Probation int
	// packets received
	Received uint32
	// packets expected in last interval
	ExpectedPrior uint32
	// packets received in last interval
	ReceivedPrior uint32
	// relative transit time for previous packet
	Transit uint32
	// estimated jitter.
	Jitter uint32
}

type RTPPacket struct {
	Seq      uint16
	Buf      []byte
	RecvTime int64
}

type RTPDemuxContext struct {
	payloadType        int
	ssrc               uint32
	seq                uint16
	timestamp          uint32
	baseTimestamp      uint32
	curTimestamp       uint32
	unwrappedTimestamp int64
	rangeStartOffset   int64
	maxPayloadSize     int
	hostname           string

	srtpEnabled bool
	// srtpContext
	statistics *RTPStatistics

	lastRtcpNtpTime       int64
	lastRtcpReceptionTime int64
	firstRtcpNtpTime      int64
	firstRtcpTimestamp    int64
	lastRtcpTimestamp     uint32
	rtcpTsOffset          int64

	prevRet int
	queue   []RTPPacket

	TimeScale       int
	DynamicProtocol DynamicProtocol
}

const RTP_SEQ_MOD = 1 << 16
const RTP_VERSION = 2
const RTP_NOTS_VALUE = (1 << 32) - 1

const (
	RTP_FLAG_KEY    = 0x1 ///< RTP packet contains a keyframe
	RTP_FLAG_MARKER = 0x2 ///< RTP marker bit was set for this packet
)

const AV_NOPTS_VALUE = 0
const AV_TIME_BASE = int64(time.Millisecond)

func relativeTime() int64 {
	return time.Now().UnixNano() / AV_TIME_BASE
}

func newRTPStatistics(baseSeq uint16) *RTPStatistics {
	return &RTPStatistics{
		Probation: 1,
		MaxSeq:    baseSeq,
	}
}

func (s *RTPStatistics) init(seq uint16) {
	s.MaxSeq = seq
	s.BaseSeq = uint32(seq - 1)
	s.BadSeq = RTP_SEQ_MOD + 1
}

func (s *RTPStatistics) validPacketInSequence(seq uint16) bool {
	udelta := seq - s.MaxSeq
	maxDropout := uint16(3000)
	maxDisorder := 100
	minSequential := 2

	/* source not valid until MIN_SEQUENTIAL packets with sequence
	* seq. numbers have been received */
	if s.Probation != 0 {
		if seq == s.MaxSeq+1 {
			s.Probation--
			s.MaxSeq = seq
			if s.Probation == 0 {
				s.init(seq)
				s.Received++
				return true
			}
		} else {
			s.Probation = minSequential - 1
			s.MaxSeq = seq
		}
	} else if udelta < maxDropout {
		// in order, with permissible gap
		if seq < s.MaxSeq {
			// sequence number wrapped; count another 64k cycles
			s.Cycles += RTP_SEQ_MOD
		}
		s.MaxSeq = seq
	} else if udelta <= uint16(RTP_SEQ_MOD-maxDisorder) {
		// sequence made a large jump...
		if uint32(seq) == s.BadSeq {
			/* two sequential packets -- assume that the other side
			* restarted without telling us; just resync. */
			s.init(seq)
		} else {
			s.BadSeq = uint32((seq + 1) & (RTP_SEQ_MOD - 1))
			return false
		}
	} else {
		// duplicate or reordered packet...
	}
	s.Received++
	return true
}

func (s *RTPStatistics) rtcpUpdateJitter(sentTimestamp uint32, arrivalTimestamp uint32) {
	// Most of this is pretty straight from RFC 3550 appendix A.8
	transit := arrivalTimestamp - sentTimestamp
	prevTransit := s.Transit
	d := int32(transit - prevTransit)
	// Doing the FFABS() call directly on the "transit - prev_transit"
	// expression doesn't work, since it's an unsigned expression. Doing the
	// transit calculation in unsigned is desired though, since it most
	// probably will need to wrap around.
	if d < 0 {
		d = -d
	}
	s.Transit = transit
	if prevTransit == 0 {
		return
	}
	s.Jitter += uint32(d - int32((s.Jitter+8)>>4))
}

func (s *RTPDemuxContext) findMissingPackets() (uint16, uint16, bool) {
	nextSeq := s.seq + 1
	if len(s.queue) == 0 || s.queue[0].Seq == nextSeq {
		return 0, 0, false
	}
	missingMask := uint16(0)
	for i := 1; i <= 16; i++ {
		missingSeq := nextSeq + uint16(i)
		var pkt *RTPPacket
		for _, e := range s.queue {
			diff := pkt.Seq - missingSeq
			if diff >= 0 {
				pkt = &e
				break
			}
		}
		if pkt == nil {
			break
		}
		if pkt.Seq == missingSeq {
			continue
		}
		missingMask |= 1 << (uint(i) - 1)
	}
	return nextSeq, missingMask, true
}

func NewRTPDemuxContext(payloadType int, queueSize int) *RTPDemuxContext {
	hn, _ := os.Hostname()
	/*
	   if (st) {
	       switch (st->codecpar->codec_id) {
	       case AV_CODEC_ID_ADPCM_G722:
	           if (st->codecpar->sample_rate == 8000)
	               st->codecpar->sample_rate = 16000;
	           break;
	       default:
	           break;
	       }
	   }
	*/
	return &RTPDemuxContext{
		payloadType:      payloadType,
		lastRtcpNtpTime:  AV_NOPTS_VALUE,
		firstRtcpNtpTime: AV_NOPTS_VALUE,
		hostname:         hn,
		queue:            make([]RTPPacket, 0, queueSize),

		statistics: newRTPStatistics(0),
	}

}

func (s *RTPDemuxContext) finalizePacket(pkt *av.Packet, timestamp uint32) {
	// if (pkt.Time != AV_NOPTS_VALUE || pkt->dts != AV_NOPTS_VALUE)
	//         return; /* Timestamp already set by depacketizer */
	if pkt.Time != time.Duration(AV_NOPTS_VALUE) {
		return
	}
	if timestamp == RTP_NOTS_VALUE {
		return
	}
	if s.lastRtcpNtpTime != AV_NOPTS_VALUE /* &&  s->ic->nb_streams > 1 */ {
		added := int64(0)
		deltaTimestamp := int64(int32(timestamp - s.lastRtcpTimestamp))
		// TODO
		// pkt->pts = s->range_start_offset + s->rtcp_ts_offset + addend +
		//                    delta_timestamp;
		pkt.Time = time.Duration(s.rangeStartOffset+s.rtcpTsOffset+added+deltaTimestamp) * time.Millisecond
		return
	}
	if s.baseTimestamp == 0 {
		s.baseTimestamp = timestamp
	}
	/* assume that the difference is INT32_MIN < x < INT32_MAX,
	 * but allow the first timestamp to exceed INT32_MAX */
	if s.timestamp == 0 {
		s.unwrappedTimestamp += int64(timestamp)
	} else {
		s.unwrappedTimestamp += int64(int32(timestamp - s.timestamp))
	}
	// fmt.Println("TS: ", s.timestamp, timestamp, (int32)(timestamp-s.timestamp))
	s.timestamp = timestamp
	// TODO
	/*
		pkt->pts     = s->unwrapped_timestamp + s->range_start_offset -
		                   s->base_timestamp;
	*/
	if s.TimeScale != 0 {
		pkt.Time = time.Duration(s.unwrappedTimestamp+s.rangeStartOffset-int64(s.baseTimestamp)) * time.Second / time.Duration(s.TimeScale)
	} else {
		fmt.Println("rtp: timescale unavailble, not packet time")
	}
}

func (s *RTPDemuxContext) rtpParsePacketInternal(pkt *av.Packet, buf []byte) int {
	/*
	   unsigned int ssrc;
	   int payload_type, seq, flags = 0;
	   int ext, csrc;
	   AVStream *st;
	   uint32_t timestamp;
	   int rv = 0;
	*/
	rv := 0
	flags := 0

	csrc := int(buf[0] & 0x0f)
	ext := int(buf[0] & 0x10)
	payloadType := buf[1] & 0x7f

	if buf[1]&0x80 != 0 {
		flags |= RTP_FLAG_MARKER
	}
	seq := binary.BigEndian.Uint16(buf[2:4])
	timestamp := binary.BigEndian.Uint32(buf[4:8])
	ssrc := binary.BigEndian.Uint32(buf[8:12])
	/* store the ssrc in the RTPDemuxContext */
	s.ssrc = ssrc

	/* NOTE: we can handle only one payload type */
	if s.payloadType != int(payloadType) {
		return -1
	}

	// only do something with this if all the rtp checks pass...
	if !s.statistics.validPacketInSequence(seq) {
		fmt.Printf("RTP: PT=%02x: bad cseq %04x expected=%04x\n",
			payloadType, seq, ((s.seq + 1) & 0xffff))
		return -1
	}

	len := len(buf)
	if buf[0]&0x20 != 0 {
		padding := int(buf[len-1])
		if len >= 12+padding {
			len -= padding
		}
	}

	bufStart := 0
	s.seq = seq
	len -= 12
	bufStart += 12

	len -= 4 * csrc
	bufStart += 4 * csrc
	if len < 0 {
		return -1
	}

	/* RFC 3550 Section 5.3.1 RTP Header Extension handling */
	if ext > 0 {
		if len < 4 {
			return -1
		}
		/* calculate the header extension length (stored as number
		 * of 32-bit words) */
		v := binary.BigEndian.Uint16(buf[bufStart+2:])
		ext = int(v+1) << 2

		if len < ext {
			return -1
		}
		// skip past RTP header extension
		len -= ext
		bufStart += ext
	}

	if s.DynamicProtocol != nil {
		timestamp, rv = s.DynamicProtocol.ParsePacket(pkt, buf[bufStart:bufStart+len], timestamp, flags)
	} else {
		return -1
	}

	// now perform timestamp things....
	s.finalizePacket(pkt, timestamp)

	return rv
}

func (s *RTPDemuxContext) rtpParseQueuedPacket(pkt *av.Packet) int {
	if len(s.queue) <= 0 {
		return -1
	}
	if !s.hasNextPacket() {
		fmt.Printf("RTP: missed %d packets\n", s.queue[0].Seq-s.seq-1)
	}
	rv := s.rtpParsePacketInternal(pkt, s.queue[0].Buf)
	copy(s.queue[0:], s.queue[1:])
	s.queue[len(s.queue)-1] = RTPPacket{}
	s.queue = s.queue[:len(s.queue)-1]

	return rv
}

func (s *RTPDemuxContext) queuePacket(buf []byte) int {
	seq := binary.BigEndian.Uint16(buf[2:4])

	var i int
	for i = 0; i < len(s.queue); i++ {
		diff := int16(seq - s.queue[i].Seq)
		if diff < 0 {
			break
		}
	}
	s.queue = append(s.queue, RTPPacket{})
	copy(s.queue[i+1:], s.queue[i:])
	s.queue[i] = RTPPacket{
		Seq:      seq,
		Buf:      buf,
		RecvTime: relativeTime(),
	}
	return 0
}

func (s *RTPDemuxContext) hasNextPacket() bool {
	return len(s.queue) > 0 && s.queue[0].Seq == uint16(s.seq+1)
}

func (s *RTPDemuxContext) rtcpParsePacket(buf []byte) int {
	return -1
}

func (s *RTPDemuxContext) rtpParseOnePacket(pkt *av.Packet, buf []byte) int {
	flags := 0
	var timestamp uint32
	rv := 0
	if buf == nil {
		/* If parsing of the previous packet actually returned 0 or an error,
		 * there's nothing more to be parsed from that packet, but we may have
		 * indicated that we can return the next enqueued packet. */
		if s.prevRet <= 0 {
			return s.rtpParseOnePacket(pkt, buf)
		}
		/* return the next packets, if any */
		if s.DynamicProtocol != nil {
			/* timestamp should be overwritten by parse_packet, if not,
			 * the packet is left with pts == AV_NOPTS_VALUE */
			timestamp = RTP_NOTS_VALUE
			timestamp, rv = s.DynamicProtocol.ParsePacket(pkt, nil, timestamp, flags)
			s.finalizePacket(pkt, timestamp)
			return rv
		}
	}

	if len(buf) < 12 {
		return -1
	}

	if (buf[0] & 0xc0) != (RTP_VERSION << 6) {
		return -1
	}
	if rtpPTIsRtcp(buf[1]) {
		return s.rtcpParsePacket(buf)
	}

	if s.TimeScale != 0 {
		received := relativeTime()
		arrivalTs := received * int64(s.TimeScale) / AV_TIME_BASE
		timestamp = binary.BigEndian.Uint32(buf[4:8])
		s.statistics.rtcpUpdateJitter(timestamp, uint32(arrivalTs))
	}

	if (s.seq == 0 && cap(s.queue) == 0) || len(s.queue) <= 1 {
		/* First packet, or no reordering */
		return s.rtpParsePacketInternal(pkt, buf)
	} else {
		// uint16_t seq = AV_RB16(buf + 2);
		seq := binary.BigEndian.Uint16(buf[2:4])
		diff := int16(seq - s.seq)
		if diff < 0 {
			/* Packet older than the previously emitted one, drop */
			fmt.Println("RTP: dropping old packet received too late")
			return -1
		} else if diff <= 1 {
			/* Correct packet */
			rv = s.rtpParsePacketInternal(pkt, buf)
			return rv
		} else {
			/* Still missing some packet, enqueue this one. */
			rv = s.queuePacket(buf)
			if rv < 0 {
				return rv
			}
			// XXX
			// *bufptr = NULL;
			/* Return the first enqueued packet if the queue is full,
			 * even if we're missing something */
			if len(s.queue) >= cap(s.queue) {
				fmt.Println("jitter buffer full")
				return s.rtpParseQueuedPacket(pkt)
			}
			return -1
		}
	}
}

// return 0 if a packet is returned, 1 if a packet is returned and more can
// follow (use buf as NULL to read the next). -1 if no packet (error or no more
// packet).
func (s *RTPDemuxContext) RtpParsePacket(buf []byte) (*av.Packet, bool) {
	var pkt av.Packet
	rv := s.rtpParseOnePacket(&pkt, buf)
	s.prevRet = rv
	for rv < 0 && s.hasNextPacket() {
		rv = s.rtpParseQueuedPacket(&pkt)
	}
	if rv > 0 {
		return &pkt, true
	} else if rv == 0 {
		return &pkt, false
	} else {
		return nil, false
	}
}

func (s *RTPDemuxContext) SetDynamicHandlerByCodecType(t av.CodecType) bool {
	switch t {
	case av.H264:
		s.DynamicProtocol = &H264DynamicProtocol{}
	case av.AAC:
		s.DynamicProtocol = &AACDynamicProtocol{}
	default:
		return false
	}
	return true
}

func (s *RTPDemuxContext) SetDynamicHandlerByStaticId(id int) bool {
	switch id {
	case 14:
		s.DynamicProtocol = &MP3DynamicProtocol{}
	default:
		return false
	}
	return true
}
