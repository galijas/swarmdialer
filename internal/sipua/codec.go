package sipua

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/hunydev/g729"
	"github.com/shenjinti/go722"
	opus "github.com/tphakala/go-opus/opus"
)

// Codec identifies one of SwarmDialer's selectable RTP payload codecs — the
// dashboard's codec dropdown sends one of these strings through to Dial/
// AutoAnswer. GSM was deliberately left out: no usable pure-Go GSM 06.10
// encoder exists (checked live 2026-09-23), and cgo-binding to system
// libgsm would break the single-static-binary deploy story for one codec.
type Codec string

const (
	CodecULaw Codec = "ulaw"
	CodecG722 Codec = "g722"
	CodecG729 Codec = "g729"
	CodecOpus Codec = "opus"
)

// DefaultCodec is what a Dial/AutoAnswer call gets if the caller doesn't
// pick one — matches the dashboard dropdown's default selection.
const DefaultCodec = CodecULaw

// codecSpec is the fixed RTP/SDP shape of one codec — payload type and
// clock rate are the two values that actually go on the wire (in the
// rtpmap and the packet header), samplesPerPacket is the timestamp step
// per packet. The audio itself comes from codecFrames.
type codecSpec struct {
	payloadType      uint8
	rtpmapName       string // e.g. "PCMU", "G722", "opus" — the token in "a=rtpmap:<pt> <name>/<clock>[/<channels>]"
	clockRateForSDP  int    // the rate that goes in the rtpmap — NOT always the real sampling rate (G.722 and Opus both have well-known RTP-spec quirks here, see below)
	channelsForSDP   int    // 0 = omit the "/<channels>" part of rtpmap (mono, implied)
	samplesPerPacket int    // RTP timestamp advance per packet, in the codec's real clock rate
}

var codecSpecs = map[Codec]codecSpec{
	// G.711 u-law: RFC 3551 static payload type 0.
	CodecULaw: {
		payloadType: 0, rtpmapName: "PCMU", clockRateForSDP: 8000,
		samplesPerPacket: rtpPacketDurationMs * 8000 / 1000, // 160
	},
	// G.722: RFC 3551 static payload type 9. Real sample rate is 16kHz, but
	// per a long-standing RTP-spec quirk (RFC 3551 §4.5.2) the rtpmap and
	// RTP timestamp clock are declared as 8000 regardless — this isn't a
	// bug, every G.722 implementation does this. 20ms @ 16kHz = 320 PCM
	// samples in, 160 encoded bytes out (SB-ADPCM packs one byte per
	// sample-pair).
	CodecG722: {
		payloadType: 9, rtpmapName: "G722", clockRateForSDP: 8000,
		samplesPerPacket: rtpPacketDurationMs * 8000 / 1000, // 160 — RTP-clock samples, not real 16kHz samples
	},
	// G.729: RFC 3551 static payload type 18. Real codec frame is 10ms (80
	// samples @ 8kHz -> 10 bytes); at our fixed 20ms ptime, two 10ms frames
	// are encoded and concatenated into each RTP packet (standard G.729
	// RTP packing — see RFC 3551 §4.5.6).
	CodecG729: {
		payloadType: 18, rtpmapName: "G729", clockRateForSDP: 8000,
		samplesPerPacket: rtpPacketDurationMs * 8000 / 1000, // 160
	},
	// Opus: no static payload type — RFC 7587 requires a dynamic one (we
	// use 111, the de facto standard used by virtually every SIP/WebRTC
	// stack) and always declares clock rate 48000 and 2 channels in the
	// rtpmap regardless of the actual encoding rate/channel count (RFC
	// 7587 §3 — another spec-mandated quirk, not a bug). We encode at 8kHz
	// mono internally (matches every other codec here — there's no real
	// wideband source audio to justify anything higher) but the RTP
	// timestamp still advances in 48000 Hz units.
	CodecOpus: {
		payloadType: 111, rtpmapName: "opus", clockRateForSDP: 48000, channelsForSDP: 2,
		samplesPerPacket: rtpPacketDurationMs * 48000 / 1000, // 960 — RTP-clock (48kHz) units
	},
}

func (c Codec) spec() codecSpec {
	spec, ok := codecSpecs[c]
	if !ok {
		return codecSpecs[DefaultCodec]
	}
	return spec
}

// IsValidCodec reports whether c is one of the selectable codecs (see the
// Codec* constants) — used by the GUI's dial handler to normalize an
// unrecognized/empty request value to DefaultCodec explicitly, rather
// than relying on spec()'s internal fallback and ending up with a
// mismatch between what's logged and what's actually on the wire.
func IsValidCodec(c Codec) bool {
	_, ok := codecSpecs[c]
	return ok
}

// rtpmapLine returns this codec's "a=rtpmap:..." SDP attribute line body
// (without the leading "a=rtpmap:" — callers already have that).
func (c Codec) rtpmapLine(payloadType uint8) string {
	s := c.spec()
	if s.channelsForSDP > 0 {
		return fmt.Sprintf("%d %s/%d/%d", payloadType, s.rtpmapName, s.clockRateForSDP, s.channelsForSDP)
	}
	return fmt.Sprintf("%d %s/%d", payloadType, s.rtpmapName, s.clockRateForSDP)
}

// --- Pre-encoded audio, shared by every call ---

// codecFrames returns the speech cycle (see speechPCM) encoded in codec c,
// one RTP payload per 20ms frame. It is encoded once, on first use, and
// shared read-only by every call: SwarmDialer simulates remote phones, so
// its own CPU should go to sending packets, not to running an encoder per
// call (one Opus encoder per call limited a test to about 300 calls on 4
// vCPUs). PBXware still receives a normal continuous stream in the codec
// and does all its usual decoding, transcoding and recording work on it.
func codecFrames(c Codec) [][]byte {
	fs, ok := frameSets[c]
	if !ok {
		c, fs = DefaultCodec, frameSets[DefaultCodec]
	}
	fs.once.Do(func() { fs.frames = encodeFrames(c) })
	return fs.frames
}

// PrepareAudio encodes every codec's audio ahead of the first call.
func PrepareAudio() {
	for c := range frameSets {
		codecFrames(c)
	}
}

type frameSet struct {
	once   sync.Once
	frames [][]byte
}

var frameSets = map[Codec]*frameSet{CodecULaw: {}, CodecG722: {}, CodecG729: {}, CodecOpus: {}}

func encodeFrames(c Codec) [][]byte {
	switch c {
	case CodecG722:
		return encodeG722(speechPCM(16000))
	case CodecG729:
		return encodeG729(speechPCM(8000))
	case CodecOpus:
		return encodeOpus(speechPCM(8000))
	default:
		return encodeULaw(speechPCM(8000))
	}
}

// pcmFrames splits pcm into 20ms frames of perFrame samples.
func pcmFrames(pcm []int16, perFrame int) [][]int16 {
	var out [][]int16
	for i := 0; i+perFrame <= len(pcm); i += perFrame {
		out = append(out, pcm[i:i+perFrame])
	}
	return out
}

// G.711 u-law: stateless, one byte per sample.
func encodeULaw(pcm []int16) [][]byte {
	var out [][]byte
	for _, fr := range pcmFrames(pcm, rtpPacketDurationMs*8000/1000) {
		b := make([]byte, len(fr))
		for i, v := range fr {
			b[i] = linearToULaw(v)
		}
		out = append(out, b)
	}
	return out
}

// G.722: stateful SB-ADPCM on 16kHz input; 320 samples in, 160 bytes out.
func encodeG722(pcm []int16) [][]byte {
	enc := go722.NewG722Encoder(go722.RateDefault, go722.G722_DEFAULT)
	var out [][]byte
	for _, fr := range pcmFrames(pcm, rtpPacketDurationMs*16000/1000) {
		le := make([]byte, len(fr)*2) // 16-bit little-endian PCM
		for i, v := range fr {
			binary.LittleEndian.PutUint16(le[i*2:], uint16(v))
		}
		out = append(out, append([]byte(nil), enc.Encode(le)...))
	}
	return out
}

// G.729: stateful CS-ACELP, two 10ms frames per 20ms RTP packet.
func encodeG729(pcm []int16) [][]byte {
	enc := g729.NewEncoder()
	frame := make([]byte, g729.FrameBytes)
	var out [][]byte
	for _, fr := range pcmFrames(pcm, rtpPacketDurationMs*8000/1000) {
		b := make([]byte, 0, g729.FrameBytes*2)
		for half := 0; half < 2; half++ {
			if err := enc.EncodeFrame(fr[half*g729.FrameSamples:(half+1)*g729.FrameSamples], frame); err != nil {
				continue // leave this sub-frame out rather than send a malformed packet
			}
			b = append(b, frame...)
		}
		out = append(out, b)
	}
	return out
}

// Opus: stateful CELT at 8kHz mono, one frame per RTP packet.
func encodeOpus(pcm []int16) [][]byte {
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 8000, Channels: 1})
	if err != nil {
		// The config is fixed and valid (8000Hz mono are both accepted); a
		// failure means the dependency itself is broken.
		panic(fmt.Sprintf("sipua: creating Opus encoder: %v", err))
	}
	buf := make([]byte, 4000)
	var out [][]byte
	for _, fr := range pcmFrames(pcm, rtpPacketDurationMs*8000/1000) {
		n, err := enc.Encode(fr, buf)
		if err != nil {
			continue
		}
		out = append(out, append([]byte(nil), buf[:n]...))
	}
	return out
}
