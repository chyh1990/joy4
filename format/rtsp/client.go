package rtsp

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/av/avutil"
	"github.com/nareix/joy4/format/rtsp/rtp"
	"github.com/nareix/joy4/format/rtsp/sdp"
)

var ErrCodecDataChange = fmt.Errorf("rtsp: codec data change, please call HandleCodecDataChange()")
var ErrTimeout = errors.New("rtsp timeout")
var ErrMaxProbe = errors.New("rtsp max probe reached")

var DebugRtp = false
var DebugRtsp = false

const (
	stageDescribeDone = iota + 1
	stageSetupDone
	stageWaitCodecData
	stageCodecDataDone
)

const RtspMaxProbeCount = 100

type udpPacket struct {
	Idx  int
	Data []byte

	Timestamp uint32
	Sequence  uint16
}

type Client struct {
	DebugRtsp bool
	DebugRtp  bool
	Headers   []string

	RtspTimeout         time.Duration
	RtpTimeout          time.Duration
	RtpKeepAliveTimeout time.Duration
	rtpKeepaliveTimer   time.Time
	rtspProbeCount      int
	// rtpKeepaliveEnterCnt int

	stage int

	// setupIdx []int
	// setupMap []int

	authHeaders func(method string) []string

	url        *url.URL
	conn       *connWithTimeout
	brconn     *bufio.Reader
	requestUri string
	cseq       uint
	streams    []*Stream
	session    string
	body       io.Reader

	// UDP
	UseUDP bool
	udpCh  chan udpPacket

	// more packets
	more          bool
	moreStreamIdx int
}

type Request struct {
	Header []string
	Uri    string
	Method string
}

type Response struct {
	StatusCode    int
	Headers       textproto.MIMEHeader
	ContentLength int
	Body          []byte

	Block []byte
}

func DialTimeout(uri string, timeout time.Duration) (self *Client, err error) {
	var URL *url.URL
	if URL, err = url.Parse(uri); err != nil {
		return
	}

	if _, _, err := net.SplitHostPort(URL.Host); err != nil {
		URL.Host = URL.Host + ":554"
	}

	dailer := net.Dialer{Timeout: timeout}
	var conn net.Conn
	if conn, err = dailer.Dial("tcp", URL.Host); err != nil {
		return
	}

	u2 := *URL
	u2.User = nil

	connt := &connWithTimeout{Conn: conn}

	self = &Client{
		conn:       connt,
		brconn:     bufio.NewReaderSize(connt, 256),
		url:        URL,
		requestUri: u2.String(),
		DebugRtp:   DebugRtp,
		DebugRtsp:  DebugRtsp,

		udpCh: make(chan udpPacket, 100),
	}
	return
}

func Dial(uri string) (self *Client, err error) {
	return DialTimeout(uri, 0)
}

func (self *Client) allCodecDataReady() bool {
	for _, s := range self.streams {
		if s.CodecData == nil {
			return false
		}
	}
	return true
}

func (self *Client) probe() (err error) {
	for {
		if self.allCodecDataReady() {
			break
		}
		if _, err = self.readPacket(); err != nil {
			return
		}
	}
	self.stage = stageCodecDataDone
	return
}

func (self *Client) prepare(stage int) (err error) {
	for self.stage < stage {
		self.rtspProbeCount++
		if self.rtspProbeCount > RtspMaxProbeCount {
			err = ErrMaxProbe
			return
		}
		switch self.stage {
		case 0:
			if _, err = self.Describe(); err != nil {
				return
			}

		case stageDescribeDone:
			if err = self.SetupAll(); err != nil {
				return
			}

		case stageSetupDone:
			if err = self.Play(); err != nil {
				return
			}

		case stageWaitCodecData:
			if err = self.probe(); err != nil {
				return
			}
		}
	}
	return
}

func (self *Client) Streams() (streams []av.CodecData, err error) {
	if err = self.prepare(stageCodecDataDone); err != nil {
		return
	}
	for _, s := range self.streams {
		streams = append(streams, s.CodecData)
	}
	return
}

func (self *Client) SendRtpKeepalive() (err error) {
	if self.RtpKeepAliveTimeout > 0 {
		if self.rtpKeepaliveTimer.IsZero() {
			self.rtpKeepaliveTimer = time.Now()
		} else if time.Now().Sub(self.rtpKeepaliveTimer) > self.RtpKeepAliveTimeout {
			self.rtpKeepaliveTimer = time.Now()
			if self.DebugRtsp {
				fmt.Println("rtp: keep alive")
			}
			req := Request{
				Method: "OPTIONS",
				Uri:    self.requestUri,
			}
			if err = self.WriteRequest(req); err != nil {
				return
			}
		}
	}
	return
}

func (self *Client) WriteRequest(req Request) (err error) {
	self.conn.Timeout = self.RtspTimeout
	self.cseq++

	buf := &bytes.Buffer{}

	fmt.Fprintf(buf, "%s %s RTSP/1.0\r\n", req.Method, req.Uri)
	fmt.Fprintf(buf, "CSeq: %d\r\n", self.cseq)

	if self.authHeaders != nil {
		headers := self.authHeaders(req.Method)
		for _, s := range headers {
			io.WriteString(buf, s)
			io.WriteString(buf, "\r\n")
		}
	}
	for _, s := range req.Header {
		io.WriteString(buf, s)
		io.WriteString(buf, "\r\n")
	}
	for _, s := range self.Headers {
		io.WriteString(buf, s)
		io.WriteString(buf, "\r\n")
	}
	io.WriteString(buf, "\r\n")

	bufout := buf.Bytes()

	if self.DebugRtsp {
		fmt.Print("> ", string(bufout))
	}

	if _, err = self.conn.Write(bufout); err != nil {
		return
	}

	return
}

func (self *Client) parseBlockHeader(h []byte) (length int, no int, timestamp uint32, seq uint16, valid bool) {
	length = int(h[2])<<8 + int(h[3])
	no = int(h[1])
	if no/2 >= len(self.streams) {
		return
	}

	if no%2 == 0 { // rtp
		if length < 8 {
			return
		}

		// V=2
		if h[4]&0xc0 != 0x80 {
			return
		}

		stream := self.streams[no/2]
		if int(h[5]&0x7f) != stream.Sdp.PayloadType {
			return
		}

		seq = binary.BigEndian.Uint16(h[6:8])
		timestamp = binary.BigEndian.Uint32(h[8:12])
	} else { // rtcp
	}

	valid = true
	return
}

func (self *Client) parseHeaders(b []byte) (statusCode int, headers textproto.MIMEHeader, err error) {
	var line string
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(b)))
	if line, err = r.ReadLine(); err != nil {
		err = fmt.Errorf("rtsp: header invalid")
		return
	}

	if codes := strings.Split(line, " "); len(codes) >= 2 {
		if statusCode, err = strconv.Atoi(codes[1]); err != nil {
			err = fmt.Errorf("rtsp: header invalid: %s", err)
			return
		}
	}

	headers, _ = r.ReadMIMEHeader()
	return
}

func (self *Client) handleResp(res *Response) (err error) {
	if sess := res.Headers.Get("Session"); sess != "" && self.session == "" {
		if fields := strings.Split(sess, ";"); len(fields) > 0 {
			self.session = fields[0]
		}
	}
	if res.StatusCode == 401 {
		if err = self.handle401(res); err != nil {
			return
		}
	}
	return
}

func (self *Client) handle401(res *Response) (err error) {
	/*
		RTSP/1.0 401 Unauthorized
		CSeq: 2
		Date: Wed, May 04 2016 10:10:51 GMT
		WWW-Authenticate: Digest realm="LIVE555 Streaming Media", nonce="c633aaf8b83127633cbe98fac1d20d87"
	*/
	authval := res.Headers.Get("WWW-Authenticate")
	hdrval := strings.SplitN(authval, " ", 2)
	var realm, nonce string

	if len(hdrval) == 2 {
		for _, field := range strings.Split(hdrval[1], ",") {
			field = strings.Trim(field, ", ")
			if keyval := strings.Split(field, "="); len(keyval) == 2 {
				key := keyval[0]
				val := strings.Trim(keyval[1], `"`)
				switch key {
				case "realm":
					realm = val
				case "nonce":
					nonce = val
				}
			}
		}

		if realm != "" {
			var username string
			var password string

			if self.url.User == nil {
				err = fmt.Errorf("rtsp: no username")
				return
			}
			username = self.url.User.Username()
			password, _ = self.url.User.Password()

			self.authHeaders = func(method string) []string {
				headers := []string{
					fmt.Sprintf(`Authorization: Basic %s`, base64.StdEncoding.EncodeToString([]byte(username+":"+password))),
				}
				if nonce != "" {
					hs1 := md5hash(username + ":" + realm + ":" + password)
					hs2 := md5hash(method + ":" + self.requestUri)
					response := md5hash(hs1 + ":" + nonce + ":" + hs2)
					headers = append(headers, fmt.Sprintf(
						`Authorization: Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
						username, realm, nonce, self.requestUri, response))
				}
				return headers
			}
		}
	}

	return
}

func (self *Client) findRTSP() (block []byte, data []byte, err error) {
	const (
		R = iota + 1
		T
		S
		Header
		Dollar
	)
	var _peek [8]byte
	peek := _peek[0:0]
	stat := 0

	for i := 0; ; i++ {
		var b byte
		if b, err = self.brconn.ReadByte(); err != nil {
			return
		}
		switch b {
		case 'R':
			if stat == 0 {
				stat = R
			}
		case 'T':
			if stat == R {
				stat = T
			}
		case 'S':
			if stat == T {
				stat = S
			}
		case 'P':
			if stat == S {
				stat = Header
			}
		case '$':
			if stat != Dollar {
				stat = Dollar
				peek = _peek[0:0]
			}
		default:
			if stat != Dollar {
				stat = 0
				peek = _peek[0:0]
			}
		}

		if false && self.DebugRtp {
			fmt.Println("rtsp: findRTSP", i, b)
		}

		if stat != 0 {
			peek = append(peek, b)
		}
		if stat == Header {
			data = peek
			return
		}

		if stat == Dollar && len(peek) >= 12 {
			if self.DebugRtp {
				fmt.Println("rtsp: dollar at", i, len(peek))
			}
			if blocklen, _, _, _, ok := self.parseBlockHeader(peek); ok {
				left := blocklen + 4 - len(peek)
				block = append(peek, make([]byte, left)...)
				if _, err = io.ReadFull(self.brconn, block[len(peek):]); err != nil {
					return
				}
				return
			}
			stat = 0
			peek = _peek[0:0]
		}
	}

	return
}

func (self *Client) readLFLF() (block []byte, data []byte, err error) {
	const (
		LF = iota + 1
		LFLF
	)
	peek := []byte{}
	stat := 0
	dollarpos := -1
	lpos := 0
	pos := 0

	for {
		var b byte
		if b, err = self.brconn.ReadByte(); err != nil {
			return
		}
		switch b {
		case '\n':
			if stat == 0 {
				stat = LF
				lpos = pos
			} else if stat == LF {
				if pos-lpos <= 2 {
					stat = LFLF
				} else {
					lpos = pos
				}
			}
		case '$':
			dollarpos = pos
		}
		peek = append(peek, b)

		if stat == LFLF {
			data = peek
			return
		} else if dollarpos != -1 && dollarpos-pos >= 12 {
			hdrlen := dollarpos - pos
			start := len(peek) - hdrlen
			if blocklen, _, _, _, ok := self.parseBlockHeader(peek[start:]); ok {
				block = append(peek[start:], make([]byte, blocklen+4-hdrlen)...)
				if _, err = io.ReadFull(self.brconn, block[hdrlen:]); err != nil {
					return
				}
				return
			}
			dollarpos = -1
		}

		pos++
	}

	return
}

func (self *Client) readResp(b []byte) (res Response, err error) {
	if res.StatusCode, res.Headers, err = self.parseHeaders(b); err != nil {
		return
	}
	res.ContentLength, _ = strconv.Atoi(res.Headers.Get("Content-Length"))
	if res.ContentLength > 0 {
		res.Body = make([]byte, res.ContentLength)
		if _, err = io.ReadFull(self.brconn, res.Body); err != nil {
			return
		}
	}
	if err = self.handleResp(&res); err != nil {
		return
	}
	return
}

func (self *Client) poll() (res Response, err error) {
	var block []byte
	var rtsp []byte
	var headers []byte

	self.conn.Timeout = self.RtspTimeout
	for {
		if block, rtsp, err = self.findRTSP(); err != nil {
			return
		}
		if len(block) > 0 {
			res.Block = block
			return
		} else {
			if block, headers, err = self.readLFLF(); err != nil {
				return
			}
			if len(block) > 0 {
				res.Block = block
				return
			}
			if res, err = self.readResp(append(rtsp, headers...)); err != nil {
				return
			}
		}
		return
	}

	return
}

func (self *Client) ReadResponse() (res Response, err error) {
	for {
		if res, err = self.poll(); err != nil {
			return
		}
		if res.StatusCode > 0 {
			return
		}
	}
	return
}

func (self *Client) SetupAll() (err error) {
	return self.Setup()
}

func (self *Client) Setup() (err error) {
	if err = self.prepare(stageDescribeDone); err != nil {
		return
	}

	for si := range self.streams {
		uri := ""
		control := self.streams[si].Sdp.Control
		if strings.HasPrefix(control, "rtsp://") {
			uri = control
		} else {
			uri = self.requestUri + "/" + control
		}
		req := Request{Method: "SETUP", Uri: uri}
		if self.UseUDP {
			err = self.streams[si].setupUDP()
			if err != nil {
				return
			}
			p1 := self.streams[si].rtpUdpPort()
			p2 := self.streams[si].rtcpUdpPort()
			req.Header = append(req.Header, fmt.Sprintf("Transport: RTP/AVP;unicast;client_port=%d-%d", p1, p2))
		} else {
			req.Header = append(req.Header, fmt.Sprintf("Transport: RTP/AVP/TCP;unicast;interleaved=%d-%d", si*2, si*2+1))
		}
		if self.session != "" {
			req.Header = append(req.Header, "Session: "+self.session)
		}
		if err = self.WriteRequest(req); err != nil {
			return
		}
		var resp Response
		if resp, err = self.ReadResponse(); err != nil {
			return
		}
		if resp.StatusCode != 200 {
			err = fmt.Errorf("rtsp: setup response code=%d", resp.StatusCode)
			return
		}

		if self.UseUDP {
			self.streams[si].sendPunch(self.requestUri, &resp)
		}
	}

	if self.stage == stageDescribeDone {
		self.stage = stageSetupDone
	}
	return
}

func md5hash(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func (self *Client) Describe() (streams []sdp.Media, err error) {
	var res Response

	for i := 0; i < 2; i++ {
		req := Request{
			Method: "DESCRIBE",
			Uri:    self.requestUri,
			Header: []string{"Accept: application/sdp"},
		}
		if err = self.WriteRequest(req); err != nil {
			return
		}
		if res, err = self.ReadResponse(); err != nil {
			return
		}
		if res.StatusCode == 200 {
			break
		}
	}
	if res.ContentLength == 0 {
		err = fmt.Errorf("rtsp: Describe failed, StatusCode=%d", res.StatusCode)
		return
	}

	body := string(res.Body)

	if self.DebugRtsp {
		fmt.Println("<", body)
	}

	_, medias := sdp.Parse(body)

	self.streams = []*Stream{}
	for i, media := range medias {
		stream := &Stream{Sdp: media, client: self, Idx: i}
		if err := stream.makeCodecData(); err != nil {
			fmt.Println("warn: stream error: ", err, media)
			continue
		}

		if self.DebugRtsp {
			fmt.Printf("rtsp: stream %d: %+v\n", i, stream)
		}
		self.streams = append(self.streams, stream)
		streams = append(streams, media)
	}

	if self.stage == 0 {
		self.stage = stageDescribeDone
	}
	return
}

func (self *Client) Options() (err error) {
	req := Request{
		Method: "OPTIONS",
		Uri:    self.requestUri,
	}
	if self.session != "" {
		req.Header = append(req.Header, "Session: "+self.session)
	}
	if err = self.WriteRequest(req); err != nil {
		return
	}
	if _, err = self.ReadResponse(); err != nil {
		return
	}
	return
}

func (self *Client) Play() (err error) {
	req := Request{
		Method: "PLAY",
		Uri:    self.requestUri,
	}
	req.Header = append(req.Header, "Session: "+self.session)
	if err = self.WriteRequest(req); err != nil {
		return
	}

	if self.allCodecDataReady() {
		self.stage = stageCodecDataDone
	} else {
		self.stage = stageWaitCodecData
	}
	return
}

func (self *Client) closeUDP() {
	for _, s := range self.streams {
		s.Close()
	}
}

func (self *Client) Teardown() (err error) {
	defer self.closeUDP()
	req := Request{
		Method: "TEARDOWN",
		Uri:    self.requestUri,
	}
	req.Header = append(req.Header, "Session: "+self.session)
	if err = self.WriteRequest(req); err != nil {
		return
	}
	return
}

func (self *Client) Close() (err error) {
	self.closeUDP()
	return self.conn.Conn.Close()
}

func (self *Client) handleBlock(block []byte) (pkt av.Packet, ok bool, err error) {
	if self.more {
		panic("more should be false")
	}
	_, blockno, _, _, _ := self.parseBlockHeader(block)
	if blockno%2 != 0 {
		if self.DebugRtp {
			fmt.Println("rtsp: rtcp block len", len(block)-4, "no", blockno)
		}
	}

	i := blockno / 2
	if i >= len(self.streams) {
		err = fmt.Errorf("rtsp: block no=%d invalid", blockno)
		return
	}
	stream := self.streams[i]

	if stream.ctx == nil {
		err = fmt.Errorf("rtsp: block no=%d demux not avaiable", blockno)
		return
	}

	/*
		if self.DebugRtp {
			l := 32
			if l > len(block) {
				l = len(block)
			}
			fmt.Println("raw")
			fmt.Print(hex.Dump(block[:l]))
		}
	*/

	var more bool
	pkt, more, err = stream.ctx.RtpParsePacket(block[4:])
	if err == rtp.ErrNoPacket {
		err = nil
		return
	}
	if err != nil {
		return
	}

	self.more = more
	if more {
		self.moreStreamIdx = i
	}

	ok = true
	pkt.Idx = int8(i)

	if self.DebugRtp {
		fmt.Println("rtp: packet len:", len(pkt.Data), "stream:", i, "time:", pkt.Time, "more:", more)
		l := 32
		if l > len(pkt.Data) {
			l = len(pkt.Data)
		}
		fmt.Print(hex.Dump(pkt.Data[:l]))
	}

	return
}

func (self *Client) rtpTimeoutChannel() <-chan time.Time {
	if self.RtpTimeout == 0 {
		return nil
	}
	return time.After(self.RtpTimeout)
}

func (self *Client) readUDPPacket() (pkt av.Packet, err error) {
	for {
		select {
		case p, ok := <-self.udpCh:
			if !ok {
				err = io.EOF
				return
			}
			var timestamp uint32
			var seq uint16
			if _, _, timestamp, seq, ok = self.parseBlockHeader(p.Data); !ok {
				continue
			}
			p.Timestamp = timestamp
			p.Sequence = seq
			// if self.DebugRtp {
			// 	fmt.Println("rtp: ", p.Timestamp, p.Sequence)
			// }
			if pkt, ok, err = self.handleBlock(p.Data); err != nil {
				fmt.Println("rtsp: bad block: ", err)
				return
			}
			if ok {
				return
			}
		case <-time.After(self.RtpTimeout):
			err = ErrTimeout
			return
		}
	}

	return
}

func (self *Client) readTCPPacket() (pkt av.Packet, err error) {
	for {
		var res Response
		for {
			if res, err = self.poll(); err != nil {
				return
			}
			if len(res.Block) > 0 {
				break
			}
		}

		var ok bool
		if pkt, ok, err = self.handleBlock(res.Block); err != nil {
			return
		}
		if ok {
			return
		}
	}
}

func (self *Client) readPacket() (pkt av.Packet, err error) {
	if err = self.SendRtpKeepalive(); err != nil {
		return
	}

	for self.more {
		pkt, self.more, err = self.streams[self.moreStreamIdx].ctx.RtpParsePacket(nil)
		if err == rtp.ErrNoPacket {
			err = nil
		} else if err != nil {
			return
		} else {
			return
		}
	}

	if self.UseUDP {
		return self.readUDPPacket()
	} else {
		return self.readTCPPacket()
	}
}

func (self *Client) ReadPacket() (pkt av.Packet, err error) {
	if err = self.prepare(stageCodecDataDone); err != nil {
		return
	}
	return self.readPacket()
}

func Handler(h *avutil.RegisterHandler) {
	h.UrlDemuxer = func(uri string) (ok bool, demuxer av.DemuxCloser, err error) {
		if !strings.HasPrefix(uri, "rtsp://") {
			return
		}
		ok = true
		demuxer, err = Dial(uri)
		return
	}
}
