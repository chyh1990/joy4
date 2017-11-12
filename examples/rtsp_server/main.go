package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/av/avutil"
	"github.com/nareix/joy4/codec/h264parser"
	"github.com/nareix/joy4/format"
	"github.com/nareix/joy4/format/rtsp"
)

func init() {
	format.RegisterAll()
}

func main() {
	srcfile := flag.String("src", "big_buck_bunny.flv", "Source file")
	flag.Parse()

	rtsp.Debug = true
	server := &rtsp.Server{
		Addr: ":8888",
	}
	src, err := avutil.Open(*srcfile)
	if err != nil {
		panic(err)
	}
	srcStreams, err := src.Streams()
	if err != nil {
		panic(err)
	}
	src.Close()
	fmt.Printf("streams %+v\n", srcStreams)

	server.HandlePublish = func(conn *rtsp.Conn, u *url.URL) ([]av.CodecData, error) {
		if strings.HasPrefix(u.Path, "/test.flv") {
			return srcStreams, nil
		}
		return nil, nil
	}
	server.HandlePlay = func(session *rtsp.Session) error {
		src, err := avutil.Open(*srcfile)
		if err != nil {
			return err
		}

		go func() {
			defer src.Close()
			defer session.Close()
			// avutil.CopyPackets(session, src)
			var err error

			<-session.Events()
			startTime := time.Now()
			for session.IsPlaying() {
				var pkt av.Packet
				if pkt, err = src.ReadPacket(); err != nil {
					if err == io.EOF {
						err = nil
						break
					}
				}
				fromStart := time.Now().Sub(startTime)
				if pkt.Idx == 0 && len(pkt.Data) > 4 {
					nalus, _ := h264parser.SplitNALUs(pkt.Data)
					for _, nal := range nalus {
						switch h264parser.NALUType(nal) {
						case h264parser.NALU_IDR_SLICE, h264parser.NALU_NON_IDR_SLICE:
							header, _ := h264parser.ParseSliceHeaderFromNALU(nal)
							fmt.Println(fromStart, "frame: ", pkt.IsKeyFrame, header)
						case h264parser.NALU_SEI:
							sei, _ := h264parser.ParseSEIMessageFromNALU(nal)
							fmt.Println(fromStart, "SEI: ", sei.Type, sei.PayloadSize)
							fmt.Print(hex.Dump(sei.Payload))
						}
						pkt.Data = nal

						if pkt.Time > fromStart {
							time.Sleep(pkt.Time - fromStart)
						}
						if err = session.WritePacket(pkt); err != nil {
							break
						}
					}
				}
			}
			fmt.Println("done: ", err)
		}()
		return nil
	}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}

	// play with ffplay: ffplay -v debug -rtsp_transport tcp rtsp://localhost:8888/test.flv
}
