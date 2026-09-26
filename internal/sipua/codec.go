package sipua

import (
	"fmt"

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
// rtpmap and the packet header), samplesPerPacket/frameEncoder govern how
// silence gets encoded into that shape.
type codecSpec struct {
	payloadType      uint8
	rtpmapName       string // e.g. "PCMU", "G722", "opus" — the token in "a=rtpmap:<pt> <name>/<clock>[/<channels>]"
	clockRateForSDP  int    // the rate that goes in the rtpmap — NOT always the real sampling rate (G.722 and Opus both have well-known RTP-spec quirks here, see below)
	channelsForSDP   int    // 0 = omit the "/<channels>" part of rtpmap (mono, implied)
	samplesPerPacket int    // RTP timestamp advance per packet, in the codec's real clock rate
	newEncoder       func() frameEncoder
}

// frameEncoder produces one RTP payload's worth of encoded silence per
// call, advancing whatever internal prediction/filter state that codec
// keeps between frames (G.722 and G.729 are stateful ADPCM/CELP codecs —
// unlike G.711, silence through them isn't a fixed byte pattern, it's
// whatever the algorithm converges to). One frameEncoder is created per
// RTPSession and reused for the life of that call.
type frameEncoder interface {
	EncodeSilenceFrame() []byte
}

var codecSpecs = map[Codec]codecSpec{
	// G.711 u-law: RFC 3551 static payload type 0. Stateless — silence is
	// always the same byte (0xFF), so this reuses the pre-existing fast path.
	CodecULaw: {
		payloadType: 0, rtpmapName: "PCMU", clockRateForSDP: 8000,
		samplesPerPacket: rtpPacketDurationMs * 8000 / 1000, // 160
		newEncoder:       func() frameEncoder { return ulawSilenceEncoder{} },
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
		newEncoder:       func() frameEncoder { return newG722SilenceEncoder() },
	},
	// G.729: RFC 3551 static payload type 18. Real codec frame is 10ms (80
	// samples @ 8kHz -> 10 bytes); at our fixed 20ms ptime, two 10ms frames
	// are encoded and concatenated into each RTP packet (standard G.729
	// RTP packing — see RFC 3551 §4.5.6).
	CodecG729: {
		payloadType: 18, rtpmapName: "G729", clockRateForSDP: 8000,
		samplesPerPacket: rtpPacketDurationMs * 8000 / 1000, // 160
		newEncoder:       func() frameEncoder { return newG729SilenceEncoder() },
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
		newEncoder:       func() frameEncoder { return newOpusSilenceEncoder() },
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

// --- G.711 u-law (stateless — fixed silence byte) ---

type ulawSilenceEncoder struct{}

func (ulawSilenceEncoder) EncodeSilenceFrame() []byte {
	b := make([]byte, codecSpecs[CodecULaw].samplesPerPacket)
	for i := range b {
		b[i] = 0xFF // u-law encoding of zero amplitude
	}
	return b
}

// --- G.722 (stateful SB-ADPCM) ---

type g722SilenceEncoder struct {
	enc       *go722.G722Encoder
	silentPCM []byte // 320 samples (20ms @ 16kHz) of zero PCM, reused every frame
}

func newG722SilenceEncoder() *g722SilenceEncoder {
	const pcmSamplesPerPacket = rtpPacketDurationMs * 16000 / 1000 // 320, real 16kHz input rate
	return &g722SilenceEncoder{
		enc:       go722.NewG722Encoder(go722.RateDefault, go722.G722_DEFAULT),
		silentPCM: make([]byte, pcmSamplesPerPacket*2), // 16-bit LE samples, all-zero == digital silence
	}
}

func (e *g722SilenceEncoder) EncodeSilenceFrame() []byte {
	return e.enc.Encode(e.silentPCM)
}

// --- G.729 (stateful CS-ACELP, two 10ms frames per 20ms RTP packet) ---

type g729SilenceEncoder struct {
	enc        *g729.Encoder
	silentPCM  []int16 // 80 samples (10ms @ 8kHz), reused every sub-frame
	frameBytes []byte  // scratch: one 10ms encoded frame (g729.FrameBytes == 10)
}

func newG729SilenceEncoder() *g729SilenceEncoder {
	return &g729SilenceEncoder{
		enc:        g729.NewEncoder(),
		silentPCM:  make([]int16, g729.FrameSamples),
		frameBytes: make([]byte, g729.FrameBytes),
	}
}

func (e *g729SilenceEncoder) EncodeSilenceFrame() []byte {
	out := make([]byte, 0, g729.FrameBytes*2)
	for i := 0; i < 2; i++ { // two 10ms G.729 frames per 20ms RTP packet
		if err := e.enc.EncodeFrame(e.silentPCM, e.frameBytes); err != nil {
			continue // leave this sub-frame out rather than send a malformed packet
		}
		out = append(out, e.frameBytes...)
	}
	return out
}

// --- Opus (stateful CELT, one frame per RTP packet) ---

type opusSilenceEncoder struct {
	enc       *opus.Encoder
	silentPCM []int16 // 160 samples (20ms @ 8kHz mono), reused every frame
	buf       []byte  // scratch encode buffer
}

func newOpusSilenceEncoder() *opusSilenceEncoder {
	const sampleRate = 8000
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: sampleRate, Channels: 1})
	if err != nil {
		// EncoderConfig here is fixed/known-valid at compile time (8000Hz,
		// mono are both in the library's accepted set) — a failure here
		// means the dependency itself is broken, not a runtime input
		// problem, so there's no sensible fallback to degrade to.
		panic(fmt.Sprintf("sipua: creating Opus encoder: %v", err))
	}
	return &opusSilenceEncoder{
		enc:       enc,
		silentPCM: make([]int16, rtpPacketDurationMs*sampleRate/1000), // 160
		buf:       make([]byte, 4000),                                 // generous — a real Opus packet is far smaller
	}
}

func (e *opusSilenceEncoder) EncodeSilenceFrame() []byte {
	n, err := e.enc.Encode(e.silentPCM, e.buf)
	if err != nil {
		return nil
	}
	return e.buf[:n]
}
