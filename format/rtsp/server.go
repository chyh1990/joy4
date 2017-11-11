package rtsp

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/h264parser"
)

var (
	Debug = false
)

type Server struct {
	Addr         string
	WriteTimeout time.Duration

	HandlePublish func(*Conn, *url.URL) ([]av.CodecData, error)
	HandlePlay    func(*Session) error

	listener *net.TCPListener
}

type Conn struct {
	mu       sync.Mutex
	netconn  net.Conn
	textconn *textproto.Conn

	cseq uint

	server *Server

	sessions map[string]*Session

	closing int32
}

type SessionEvent int32

const (
	SessionPause SessionEvent = iota + 1
	SessionPlay
	SessionTeardown
)

const (
	h264DynamincType = 96
)

type Session struct {
	Uri  *url.URL
	Conn *Conn

	event chan SessionEvent

	session string
	seq     uint16
	streams []av.CodecData
	state   int32
}

type response struct {
	lines []string
	body  []byte
}

func newConn(conn net.Conn) *Conn {
	return &Conn{
		netconn:  conn,
		textconn: textproto.NewConn(conn),
		sessions: make(map[string]*Session),
	}
}

func (self *Session) IsPlaying() bool {
	return atomic.LoadInt32(&self.state) == int32(SessionPlay)
}

func (self *Session) Events() <-chan SessionEvent {
	return self.event
}

func (self *Session) Streams() (streams []av.CodecData, err error) {
	return self.streams, nil
}

func (self *Session) writeFLVH264Packet(pkt av.Packet) (err error) {
	/*
		FU-A H264 https://tools.ietf.org/html/rfc3984

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

	if len(pkt.Data) < 1 {
		return
	}
	// strip first 4 bytes for flv nalu size
	data := pkt.Data
	// fmt.Println("write ", pkt.Idx, pkt.Time, len(pkt.Data))

	nalFirst := data[0]
	nalType := nalFirst & 0x1f

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
	if nalType > 6 {
		return
	}

	first := true
	maxFragmentSize := 65000
	for start := 1; start < len(data); start += maxFragmentSize {
		w := new(bytes.Buffer)
		fragSize := len(data) - start
		if fragSize > maxFragmentSize {
			fragSize = maxFragmentSize
		}

		// rtp header
		err = binary.Write(w, binary.BigEndian, uint8(0x80))
		err = binary.Write(w, binary.BigEndian, uint8(h264DynamincType))
		// sequence
		err = binary.Write(w, binary.BigEndian, uint16(self.seq))
		self.seq++
		// timestamp (90k / fps)
		// XXX timestamp should start randomly
		t := (pkt.Time * 90000 / time.Second)
		err = binary.Write(w, binary.BigEndian, uint32(t))
		// SSRC
		err = binary.Write(w, binary.BigEndian, rand.Int31())

		// fmt.Print(hex.Dump(data[:4]))
		fuIndicator := byte((nalFirst & 0xe0) | 28)
		fuHeader := uint8(nalFirst & 0x1f)
		if first {
			fuHeader |= 0x80
			first = false
		}
		end := (start + fragSize) == len(data)
		if end {
			fuHeader |= 0x40
		}
		err = w.WriteByte(fuIndicator)
		err = w.WriteByte(fuHeader)
		_, err = w.Write(data[start : start+fragSize])
		if err != nil {
			return
		}

		err = self.Conn.writeEmbeded(2*pkt.Idx, w.Bytes())
		if err != nil {
			return
		}
	}
	return

}

func (self *Session) WritePacket(pkt av.Packet) (err error) {
	if !self.IsPlaying() {
		return
	}

	if int(pkt.Idx) >= len(self.streams) {
		return
	}
	if self.streams[pkt.Idx].Type() != av.H264 {
		return
	}

	err = self.writeFLVH264Packet(pkt)
	return
}

func (self *Session) WriteTrailer() (err error) {
	return
}

func (self *Session) WriteHeader(streams []av.CodecData) (err error) {
	return
}

func (self *Session) Close() (err error) {
	return
}

func (self *Conn) Close() (err error) {
	if atomic.SwapInt32(&self.closing, 1) == 0 {
		err = self.netconn.Close()
	}
	return
}

func (self *Conn) writeStatus(code int, status string) {
	if Debug {
		fmt.Println("rtsp: write status: ", code)
	}
	lines := []string{
		fmt.Sprintf("RTSP/1.0 %d %s", code, status),
		fmt.Sprintf("CSeq: %d", self.cseq),
		fmt.Sprintf("Public: OPTIONS, DESCRIBE, SETUP, TEARDOWN, PLAY"),
	}
	self.writeResponse(response{
		lines: lines,
	})
}

func (self *Conn) publish(uri *url.URL, headers textproto.MIMEHeader) ([]av.CodecData, error) {
	var streams []av.CodecData
	var err error
	if self.server.HandlePublish != nil {
		streams, err = self.server.HandlePublish(self, uri)
		if err != nil {
			self.writeStatus(400, "Bad Request")
			return nil, err
		}
	}
	if streams == nil {
		self.writeStatus(400, "Bad Request")
		return nil, nil
	}
	return streams, nil
}

func (self *Conn) doOptions(uri *url.URL, headers textproto.MIMEHeader) error {
	if _, err := self.publish(uri, headers); err != nil {
		return err
	}
	self.writeStatus(200, "OK")
	return nil
}

func sdpType(t av.CodecType) string {
	switch t {
	case av.H264:
		return "video"
	case av.AAC:
		return "audio"
	case av.PCM_MULAW:
		return "audio"
	case av.PCM_ALAW:
		return "audio"
	}
	return "unknown"
}

func (self *Conn) doDescribe(uri *url.URL, headers textproto.MIMEHeader) error {
	streams, err := self.publish(uri, headers)
	if err != nil {
		return err
	}
	sdps := make([]string, 0)
	/*
		v=0
		o=- 0 0 IN IP4 127.0.0.1
		s=No Title
		c=IN IP4 0.0.0.0
		t=0 0
		a=tool:libavformat 57.71.100
		m=video 0 RTP/AVP 96
		a=rtpmap:96 H264/90000
		a=fmtp:96 packetization-mode=1; sprop-parameter-sets=Z2QAHqzZQKAv+WEAAAMAAQAAAwAwDxYtlg==,aOviSyLA; profile-level-id=64001E
		a=control:streamid=0
		m=audio 0 RTP/AVP 14
		b=AS:64
		a=control:streamid=1
	*/
	sdps = append(sdps, "v=0")
	for i := range streams {
		if streams[i].Type() != av.H264 {
			if Debug {
				fmt.Println("unsupported stream type")
			}
			continue
		}
		ty, ok := streams[i].(h264parser.CodecData)
		if !ok {
			continue
		}
		fmt.Printf("%+v\n", streams[i])
		sdps = append(sdps, fmt.Sprintf("m=%s 0 RTP/AVP %d", sdpType(streams[i].Type()), h264DynamincType))
		sdps = append(sdps, fmt.Sprintf("a=control:streamid=%d", i))
		sdps = append(sdps, fmt.Sprintf("a=rtpmap:%d H264/90000", h264DynamincType))
		sps := base64.StdEncoding.EncodeToString(ty.SPS())
		pps := base64.StdEncoding.EncodeToString(ty.PPS())
		sdps = append(sdps, fmt.Sprintf("a=fmtp:%d packetization-mode=1; sprop-parameter-sets=%s", h264DynamincType, strings.Join([]string{sps, pps}, ",")))
	}
	buf := strings.Join(sdps, "\n")
	lines := []string{
		"RTSP/1.0 200 OK",
		fmt.Sprintf("CSeq: %d", self.cseq),
		fmt.Sprintf("Content-Base: %s/", uri.String()),
		"Content-Type: application/sdp",
		fmt.Sprintf("Content-Length: %d", len(buf)),
	}
	self.writeResponse(response{
		lines: lines,
		body:  []byte(buf),
	})

	return nil
}

func (self *Conn) doSetup(uri *url.URL, headers textproto.MIMEHeader) error {
	qs := uri.Query()
	idx := 0
	if t, ok := qs["streamid"]; ok {
		if len(t) > 0 {
			idx, _ = strconv.Atoi(t[0])
		}
	}
	// check
	streams, err := self.publish(uri, headers)
	if err != nil {
		return err
	}
	var sessionID string
	for {
		sessionID = fmt.Sprint(rand.Int())
		if _, ok := self.sessions[sessionID]; !ok {
			break
		}
	}
	session := Session{
		Uri:     uri,
		Conn:    self,
		event:   make(chan SessionEvent, 8),
		session: sessionID,
		seq:     uint16(rand.Int() % 65536),
		streams: streams,
		state:   int32(SessionPause),
	}
	self.sessions[sessionID] = &session

	lines := []string{
		"RTSP/1.0 200 OK",
		fmt.Sprintf("CSeq: %d", self.cseq),
		fmt.Sprintf("Session: %s", sessionID),
		fmt.Sprintf("Transport: RTP/AVP/TCP;unicast;interleaved=%d-%d", idx, idx+1),
	}
	self.writeResponse(response{
		lines: lines,
	})

	return nil

}

func (self *Conn) findSession(headers textproto.MIMEHeader) *Session {
	sessionID, ok := headers["Session"]
	if !ok || len(sessionID) < 1 {
		return nil
	}
	session, ok := self.sessions[sessionID[0]]
	if !ok {
		return nil
	}

	return session
}

func (self *Conn) doPlay(uri *url.URL, headers textproto.MIMEHeader) (err error) {
	session := self.findSession(headers)
	if session == nil {
		self.writeStatus(400, "Bad Request")
		return
	}
	if self.server.HandlePlay != nil {
		err = self.server.HandlePlay(session)
		if err != nil {
			self.writeStatus(500, "Internal")
			return
		}
	}

	self.writeStatus(200, "OK")
	atomic.StoreInt32(&session.state, int32(SessionPlay))
	session.event <- SessionPlay
	return
}

func (self *Conn) doTeardown(uri *url.URL, headers textproto.MIMEHeader) (err error) {
	session := self.findSession(headers)
	if session == nil {
		self.writeStatus(400, "Bad Request")
		return
	}
	atomic.StoreInt32(&session.state, int32(SessionTeardown))
	session.event <- SessionTeardown
	close(session.event)
	delete(self.sessions, session.session)
	self.writeStatus(200, "OK")
	return
}

func (self *Conn) dispatch(line string) error {
	var cmd, raw, proto string
	n, err := fmt.Sscanf(line, "%s %s %s", &cmd, &raw, &proto)
	if err != nil {
		return err
	}
	if n != 3 || proto != "RTSP/1.0" {
		return errors.New("invalid request")
	}
	uri, err := url.Parse(raw)
	if err != nil {
		self.writeStatus(400, "Bad Request")
		return nil
	}

	headers, err := self.textconn.ReadMIMEHeader()
	if err != nil {
		return err
	}
	if Debug {
		fmt.Println(line, headers)
	}

	if cseq, ok := headers["Cseq"]; ok {
		if len(cseq) > 0 {
			cseq, err := strconv.Atoi(cseq[0])
			if err != nil {
				return err
			}
			self.cseq = uint(cseq)
		}
	}

	switch cmd {
	case "OPTIONS":
		err = self.doOptions(uri, headers)
	case "DESCRIBE":
		err = self.doDescribe(uri, headers)
	case "SETUP":
		err = self.doSetup(uri, headers)
	case "PLAY":
		err = self.doPlay(uri, headers)
	case "TEARDOWN":
		err = self.doTeardown(uri, headers)
	default:
		self.writeStatus(400, "Bad Request")
	}
	return err
}

func (self *Conn) writeEmbeded(id int8, v []byte) (err error) {
	self.mu.Lock()
	defer self.mu.Unlock()

	if self.server.WriteTimeout != 0 {
		self.netconn.SetWriteDeadline(time.Now().Add(self.server.WriteTimeout))
	}

	w := self.textconn.W
	// frame header
	if len(v) > 65535 {
		err = errors.New("rtp frame too large")
		return
	}
	err = binary.Write(w, binary.BigEndian, uint8(0x24))
	err = binary.Write(w, binary.BigEndian, id)
	size := uint16(len(v))
	err = binary.Write(w, binary.BigEndian, size)

	_, err = w.Write(v)
	if err != nil {
		return
	}
	err = self.textconn.W.Flush()
	return
}

func (self *Conn) writeResponse(v response) (err error) {
	self.mu.Lock()
	defer self.mu.Unlock()

	if self.server.WriteTimeout != 0 {
		self.netconn.SetWriteDeadline(time.Now().Add(self.server.WriteTimeout))
	}

	for _, lines := range v.lines {
		err = self.textconn.PrintfLine(lines)
		if err != nil {
			break
		}
	}
	err = self.textconn.PrintfLine("")
	if err != nil {
		return
	}
	if v.body != nil {
		_, err = self.textconn.W.Write(v.body)
		if err != nil {
			return
		}
		err = self.textconn.W.Flush()
		if err != nil {
			return
		}
	}
	return
}

func (self *Conn) readLoop() error {
	var err error
	var line string
	for {
		line, err = self.textconn.ReadLine()
		fmt.Println(line)
		if err != nil {
			break
		}
		err = self.dispatch(line)
		if err != nil {
			break
		}
	}
	return err
}

func (self *Server) handleConn(conn *Conn) (err error) {
	err = conn.readLoop()

	for _, v := range conn.sessions {
		atomic.StoreInt32(&v.state, int32(SessionTeardown))
		v.event <- SessionTeardown
		close(v.event)
	}

	if atomic.SwapInt32(&conn.closing, 1) == 0 {
		conn.netconn.Close()
	}
	return
}

func (self *Server) ListenAndServe() (err error) {
	addr := self.Addr
	if addr == "" {
		addr = ":554"
	}
	var tcpaddr *net.TCPAddr
	if tcpaddr, err = net.ResolveTCPAddr("tcp", addr); err != nil {
		err = fmt.Errorf("rtsp: ListenAndServe: %s", err)
		return
	}

	var listener *net.TCPListener
	if listener, err = net.ListenTCP("tcp", tcpaddr); err != nil {
		return
	}
	self.listener = listener

	if Debug {
		fmt.Println("rtsp: server: listening on", addr)
	}

	for {
		var netconn net.Conn
		if netconn, err = listener.Accept(); err != nil {
			fmt.Println("rtsp: server: ", err)
			return
		}

		if Debug {
			fmt.Println("rtsp: server: accepted")
		}

		conn := newConn(netconn)
		conn.server = self
		go func() {
			err := self.handleConn(conn)
			if Debug {
				fmt.Println("rtsp: server: client closed err:", err)
			}
		}()
	}
}

func (self *Server) Close() (err error) {
	if self.listener == nil {
		return nil
	}
	return self.listener.Close()
}
