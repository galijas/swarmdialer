package sipua

import (
	"testing"
	"time"
)

func TestJitterIgnoresTimestampJumps(t *testing.T) {
	s := &RTPSession{spec: codecSpecs[CodecOpus], sessionStart: time.Now()}
	ts := uint32(4_294_000_000) // wraps within the test
	seq := uint16(65_000)
	for i := 0; i < 200; i++ {
		if i == 100 {
			ts += 1_000_000_000 // PBXware jumps its timestamps
		}
		s.onRTPReceived(seq, ts, 42)
		seq++
		ts += 960
		time.Sleep(time.Millisecond)
	}
	ms := s.MediaStats()
	if ms.JitterMS > 50 {
		t.Errorf("jitter %.1f ms: the timestamp jump or wrap was counted", ms.JitterMS)
	}
	if ms.Expected != 200 || ms.Received != 200 {
		t.Errorf("expected/received %d/%d, want 200/200 (sequence wrap)", ms.Expected, ms.Received)
	}

	// A new SSRC starts the statistics over.
	for i := 0; i < 10; i++ {
		s.onRTPReceived(uint16(i), uint32(i*960), 43)
	}
	if ms := s.MediaStats(); ms.Expected != 10 || ms.Received != 10 {
		t.Errorf("after an SSRC change: expected/received %d/%d, want 10/10", ms.Expected, ms.Received)
	}
}
