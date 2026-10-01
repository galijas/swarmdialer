package sipua

import (
	"math"
	"testing"
)

func TestSpeechPCM(t *testing.T) {
	for _, rate := range []int{8000, 16000} {
		pcm := speechPCM(rate)
		if len(pcm) != speechCycleSeconds*rate {
			t.Fatalf("%d Hz: %d samples, want %d", rate, len(pcm), speechCycleSeconds*rate)
		}
		// Share of 20ms frames that carry talk (well above the noise floor).
		per := rate / 50
		talk := 0
		for i := 0; i+per <= len(pcm); i += per {
			sum := 0.0
			for _, v := range pcm[i : i+per] {
				sum += float64(v) * float64(v)
			}
			if math.Sqrt(sum/float64(per)) > 300 {
				talk++
			}
		}
		share := float64(talk) / float64(len(pcm)/per)
		t.Logf("%d Hz: %.0f%% of frames carry talk", rate, 100*share)
		if share < 0.35 || share > 0.75 {
			t.Errorf("%d Hz: talk share %.2f, want about 0.55", rate, share)
		}
	}
}

func TestCodecFrames(t *testing.T) {
	want := map[Codec]int{CodecULaw: 160, CodecG722: 160, CodecG729: 20, CodecOpus: 0}
	for c, size := range want {
		frames := codecFrames(c)
		if len(frames) != speechCycleSeconds*1000/rtpPacketDurationMs {
			t.Fatalf("%s: %d frames", c, len(frames))
		}
		total := 0
		for i, f := range frames {
			if len(f) == 0 || (size > 0 && len(f) != size) {
				t.Fatalf("%s: frame %d has %d bytes, want %d", c, i, len(f), size)
			}
			total += len(f)
		}
		t.Logf("%s: average %.1f bytes per frame", c, float64(total)/float64(len(frames)))
	}
}
