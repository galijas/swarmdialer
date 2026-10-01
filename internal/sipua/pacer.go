package sipua

import (
	"sync"
	"time"
)

// pacerSlots spreads the sessions over the 20ms packet interval, one slot
// per millisecond, so packets leave evenly (like independent phones)
// rather than all at once.
const pacerSlots = rtpPacketDurationMs

// pacer sends every running session's RTP packets every 20ms from one
// goroutine per slot, instead of a timer and goroutine wake-up per
// session: at 512 calls (1024 sessions) that was over 50,000 wake-ups a
// second. Each session stays in one slot for its whole life, so its own
// packets are still exactly 20ms apart.
type pacer struct {
	once  sync.Once
	mu    sync.Mutex
	next  int
	slots [pacerSlots]*pacerSlot
}

type pacerSlot struct {
	mu       sync.Mutex
	sessions map[*RTPSession]struct{}
}

var mediaPacer = &pacer{}

func (p *pacer) start() {
	for i := range p.slots {
		p.slots[i] = &pacerSlot{sessions: map[*RTPSession]struct{}{}}
	}
	t0 := time.Now()
	for i, sl := range p.slots {
		go sl.run(t0.Add(time.Duration(i) * time.Millisecond))
	}
}

// add starts sending s's packets.
func (p *pacer) add(s *RTPSession) {
	p.once.Do(p.start)
	p.mu.Lock()
	sl := p.slots[p.next]
	p.next = (p.next + 1) % pacerSlots
	p.mu.Unlock()
	sl.mu.Lock()
	sl.sessions[s] = struct{}{}
	sl.mu.Unlock()
	s.mu.Lock()
	s.slot = sl
	s.mu.Unlock()
}

// remove stops sending s's packets.
func (p *pacer) remove(s *RTPSession) {
	s.mu.Lock()
	sl := s.slot
	s.slot = nil
	s.mu.Unlock()
	if sl != nil {
		sl.mu.Lock()
		delete(sl.sessions, s)
		sl.mu.Unlock()
	}
}

func (sl *pacerSlot) run(first time.Time) {
	time.Sleep(time.Until(first))
	ticker := time.NewTicker(rtpPacketDurationMs * time.Millisecond)
	defer ticker.Stop()
	var batch []*RTPSession
	for range ticker.C {
		batch = batch[:0]
		sl.mu.Lock()
		for s := range sl.sessions {
			batch = append(batch, s)
		}
		sl.mu.Unlock()
		for _, s := range batch {
			s.sendPacket()
		}
	}
}
