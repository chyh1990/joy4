package rtp

const (
	RTCP_FIR     = iota + 192
	RTCP_NACK    // 193
	RTCP_SMPTETC // 194
	RTCP_IJ      // 195
)

const (
	RTCP_SR    = iota + 200
	RTCP_RR    // 201
	RTCP_SDES  // 202
	RTCP_BYE   // 203
	RTCP_APP   // 204
	RTCP_RTPFB // 205
	RTCP_PSFB  // 206
	RTCP_XR    // 207
	RTCP_AVB   // 208
	RTCP_RSI   // 209
	RTCP_TOKEN // 210
)

func rtpPTIsRtcp(b byte) bool {
	return (b >= RTCP_FIR && b <= RTCP_IJ) ||
		b >= RTCP_SR && b <= RTCP_TOKEN
}
