package sipua

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// buildSDP produces a minimal single-audio-stream SDP body offering/
// answering exactly one codec (see codec.go) on rtpPort. It doesn't
// negotiate anything fancy — that's all SwarmDialer needs for
// silence-payload load testing.
//
// sendMedia controls the direction attribute: true → "sendrecv" (normal
// two-way silence RTP), false → "inactive" (signaling-only call, no media
// either direction — the standard SDP convention for this, so both a real
// SIP stack and our own peer will honor it symmetrically, unlike e.g.
// silently dropping the media line, which some stacks reject).
func buildSDP(localIP string, rtpPort int, sendMedia bool, codec Codec) []byte {
	direction := "inactive"
	if sendMedia {
		direction = "sendrecv"
	}
	spec := codec.spec()
	return []byte(fmt.Sprintf(
		"v=0\r\n"+
			"o=- 0 0 IN IP4 %s\r\n"+
			"s=SwarmDialer\r\n"+
			"c=IN IP4 %s\r\n"+
			"t=0 0\r\n"+
			"m=audio %d RTP/AVP %d\r\n"+
			"a=rtpmap:%s\r\n"+
			"a=%s\r\n",
		localIP, localIP, rtpPort, spec.payloadType, codec.rtpmapLine(), direction,
	))
}

// rtpmapNameToCodec maps an SDP rtpmap codec token (case-insensitive) back
// to our Codec type — used by parseSDPCodec on the answering side, which
// has to match whatever codec the caller's offer actually specified rather
// than assuming a default, now that buildSDP offers exactly one codec per
// call instead of always ulaw+alaw.
var rtpmapNameToCodec = map[string]Codec{
	"pcmu": CodecULaw,
	"g722": CodecG722,
	"g729": CodecG729,
	"opus": CodecOpus,
}

// parseSDPCodec extracts which Codec an SDP offer/answer's rtpmap line
// specifies. SwarmDialer never offers more than one codec (see buildSDP),
// so there's exactly one rtpmap line to find.
func parseSDPCodec(body []byte) (Codec, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "a=rtpmap:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "a=rtpmap:"))
		if len(fields) < 2 {
			continue
		}
		name := strings.ToLower(strings.SplitN(fields[1], "/", 2)[0])
		if c, ok := rtpmapNameToCodec[name]; ok {
			return c, nil
		}
	}
	return "", fmt.Errorf("no recognized codec rtpmap in SDP")
}

// sdpWantsMedia reports whether a received SDP body's direction attribute
// calls for media to actually flow. Defaults to true (sendrecv) if no
// direction attribute is present, per SDP convention.
func sdpWantsMedia(body []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "a=inactive" {
			return false
		}
	}
	return true
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
