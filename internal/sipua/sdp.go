package sipua

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

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

// parseSDPMedia extracts the peer's audio media address from an SDP body:
// the connection IP (from the top-level "c=" line — we don't handle a
// per-media "c=" override, since we never send one) and the port from the
// "m=audio" line. That's all SwarmDialer needs to know where to send RTP.
func parseSDPMedia(body []byte) (ip string, port int, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "c=IN IP4 "):
			ip = strings.TrimSpace(strings.TrimPrefix(line, "c=IN IP4 "))
		case strings.HasPrefix(line, "m=audio "):
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return "", 0, fmt.Errorf("malformed m=audio line: %q", line)
			}
			port, err = strconv.Atoi(fields[1])
			if err != nil {
				return "", 0, fmt.Errorf("parsing audio port from %q: %w", line, err)
			}
		}
	}
	if ip == "" {
		return "", 0, fmt.Errorf("no c=IN IP4 line found in SDP")
	}
	if port == 0 {
		return "", 0, fmt.Errorf("no m=audio line found in SDP")
	}
	return ip, port, nil
}
