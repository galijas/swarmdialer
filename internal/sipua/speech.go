package sipua

import (
	"math"
	"math/rand/v2"
)

// speechCycleSeconds is the length of the synthetic speech every call
// plays in a loop (each call starts at a random point in it).
const speechCycleSeconds = 30

// speechPCM synthesizes speechCycleSeconds of speech-like audio at the
// given sample rate (8000 or 16000), so PBXware's transcoding, recording
// and MP3 conversion work on audio shaped like a conversation rather than
// digital silence, which codecs process more cheaply than speech.
//
// It is not real speech, but has the properties that drive codec cost:
// talk spurts of 0.8-2.5s separated by 0.4-1.8s pauses (about 55% talk,
// one side of a conversation), syllables at about 4-5 per second, a voiced
// source (gliding pitch, harmonics up to 3.4kHz) shaped by vowel-like
// formants, unvoiced noise bursts for consonants, and a low background
// noise floor instead of exact silence during pauses (a real microphone
// never sends digital zero). The output is deterministic, so every run
// sends the same audio.
func speechPCM(rate int) []int16 {
	n := speechCycleSeconds * rate
	out := make([]float64, n)
	rng := rand.New(rand.NewPCG(1, 2))
	dt := 1 / float64(rate)

	// Vowel formants (Hz): /a/, /e/, /i/, /o/, /u/.
	vowels := [][2]float64{{730, 1090}, {530, 1840}, {270, 2290}, {570, 840}, {300, 870}}
	formantGain := func(f float64, v [2]float64) float64 {
		g := 0.0
		for i, fc := range v {
			bw := 90.0 + 40*float64(i)
			x := (f - fc) / bw
			g += 1 / (1 + x*x) / float64(i+1)
		}
		return g + 0.02 // a little energy everywhere, like a real vocal tract
	}

	pos := 0
	for pos < n {
		// One talk spurt, made of syllables.
		spurtEnd := min(n, pos+int((0.8+1.7*rng.Float64())*float64(rate)))
		f0 := 105 + 80*rng.Float64() // speaker pitch for this spurt
		for pos < spurtEnd {
			sylLen := int((0.14 + 0.12*rng.Float64()) * float64(rate))
			v := vowels[rng.IntN(len(vowels))]
			consonant := rng.Float64() < 0.6
			pitchStart, pitchEnd := f0*(0.9+0.2*rng.Float64()), f0*(0.85+0.2*rng.Float64())
			level := 0.5 + 0.5*rng.Float64()
			phase := 0.0
			for i := 0; i < sylLen && pos+i < spurtEnd; i++ {
				t := float64(i) / float64(sylLen)
				env := math.Pow(math.Sin(math.Pi*t), 0.6) * level
				pitch := pitchStart + (pitchEnd-pitchStart)*t
				phase += 2 * math.Pi * pitch * dt
				s := 0.0
				for h := 1; float64(h)*pitch < 3400 && float64(h)*pitch < float64(rate)/2; h++ {
					s += formantGain(float64(h)*pitch, v) * math.Sin(float64(h)*phase) / math.Sqrt(float64(h))
				}
				if consonant && t < 0.18 { // fricative/plosive onset
					s = s*t/0.18 + (rng.Float64()*2-1)*1.2*(1-t/0.18)
				}
				out[pos+i] = s * env
			}
			pos += sylLen
			pos += int((0.01 + 0.04*rng.Float64()) * float64(rate)) // short gap between syllables
		}
		pos = min(n, spurtEnd+int((0.4+1.4*rng.Float64())*float64(rate))) // pause
	}

	// Normalize talk to about -20 dBFS RMS and add the background noise
	// floor (about -60 dBFS).
	sum, cnt := 0.0, 0
	for _, s := range out {
		if s != 0 {
			sum += s * s
			cnt++
		}
	}
	gain := 1.0
	if cnt > 0 {
		gain = 0.1 * 32767 / math.Sqrt(sum/float64(cnt))
	}
	pcm := make([]int16, n)
	for i, s := range out {
		v := s*gain + rng.NormFloat64()*33
		pcm[i] = int16(max(-32768, min(32767, math.Round(v))))
	}
	return pcm
}

// linearToULaw encodes one 16-bit PCM sample as G.711 u-law.
func linearToULaw(sample int16) byte {
	const bias, clip = 0x84, 32635
	s := int(sample)
	sign := 0
	if s < 0 {
		s, sign = -s, 0x80
	}
	s = min(s, clip) + bias
	exp := 7
	for mask := 0x4000; s&mask == 0 && exp > 0; mask >>= 1 {
		exp--
	}
	mantissa := (s >> (exp + 3)) & 0x0F
	return ^byte(sign | exp<<4 | mantissa)
}
