package sipua

import "fmt"

// buildSDP produces a minimal single-audio-stream SDP body offering/answering
// G.711 (ulaw/alaw) on rtpPort. It doesn't negotiate anything fancy — that's
// all SwarmDialer needs for silence-payload load testing.
func buildSDP(localIP string, rtpPort int) []byte {
	return []byte(fmt.Sprintf(
		"v=0\r\n"+
			"o=- 0 0 IN IP4 %s\r\n"+
			"s=SwarmDialer\r\n"+
			"c=IN IP4 %s\r\n"+
			"t=0 0\r\n"+
			"m=audio %d RTP/AVP 0 8\r\n"+
			"a=rtpmap:0 PCMU/8000\r\n"+
			"a=rtpmap:8 PCMA/8000\r\n"+
			"a=sendrecv\r\n",
		localIP, localIP, rtpPort,
	))
}
