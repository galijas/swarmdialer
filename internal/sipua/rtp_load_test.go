package sipua

import (
	"context"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestRTPLoad measures SwarmDialer's own CPU cost of media: N calls (two
// RTP sessions each, sending to each other over loopback, like a remote
// call's caller and callee legs) for a few seconds. Skipped unless
// SWARMDIALER_RTP_LOAD is set, e.g.:
//
//	SWARMDIALER_RTP_LOAD=300 SWARMDIALER_RTP_CODEC=opus go test -run TestRTPLoad -v ./internal/sipua
func TestRTPLoad(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("SWARMDIALER_RTP_LOAD"))
	if n <= 0 {
		t.Skip("set SWARMDIALER_RTP_LOAD=<calls> to run")
	}
	codec := Codec(os.Getenv("SWARMDIALER_RTP_CODEC"))
	if !IsValidCodec(codec) {
		codec = CodecULaw
	}
	const dur = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sessions []*RTPSession
	for i := 0; i < n; i++ {
		a, err := NewRTPSession(0, codec)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewRTPSession(0, codec)
		if err != nil {
			t.Fatal(err)
		}
		a.SetRemote("127.0.0.1", b.LocalPort())
		b.SetRemote("127.0.0.1", a.LocalPort())
		sessions = append(sessions, a, b)
	}
	for _, s := range sessions {
		s.Start(ctx)
	}
	time.Sleep(time.Second) // settle
	cpu := func() time.Duration {
		var ru syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	var sent0, recv0 uint64
	for _, s := range sessions {
		a, b := s.Stats()
		sent0, recv0 = sent0+a, recv0+b
	}
	c0, t0 := cpu(), time.Now()
	time.Sleep(dur)
	c1, el := cpu(), time.Since(t0)
	var sent1, recv1 uint64
	for _, s := range sessions {
		a, b := s.Stats()
		sent1, recv1 = sent1+a, recv1+b
	}
	for _, s := range sessions {
		s.Stop()
	}
	expected := float64(len(sessions)) * el.Seconds() * 1000 / rtpPacketDurationMs
	t.Logf("%d calls (%s): CPU %.1f%% of one core, sent %.1f%% / received %.1f%% of expected packets",
		n, codec, 100*(c1-c0).Seconds()/el.Seconds(),
		100*float64(sent1-sent0)/expected, 100*float64(recv1-recv0)/expected)
}
