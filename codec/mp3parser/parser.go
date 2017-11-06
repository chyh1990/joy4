package mp3parser

import (
	"bytes"

	"github.com/nareix/joy4/av"
)

type CodecData struct {
	Record []byte
	Header FrameHeader
}

func (self CodecData) Type() av.CodecType {
	return av.MP3
}

func (self CodecData) ChannelLayout() av.ChannelLayout {
	switch self.Header.ChannelMode() {
	case SingleChannel:
		return av.CH_MONO
	default:
		return av.CH_STEREO
	}
}

func (self CodecData) SampleRate() int {
	return int(self.Header.SampleRate())
}

func (self CodecData) BitRate() int {
	return int(self.Header.BitRate())
}

func NewCodecDataFromMP3AudioConfigBytes(config []byte) (self CodecData, err error) {
	reader := bytes.NewReader(config)
	decoder := NewDecoder(reader)
	f := Frame{}
	skipped := 0
	err = decoder.Decode(&f, &skipped)
	if err != nil {
		return
	}
	self.Record = config
	self.Header = f.Header()
	return
}
