package main

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"swarmdialer/internal/pbxware"
	"swarmdialer/internal/store"
)

// trunkCodecSettle is how long to wait after reordering trunk codecs before
// dialing. PBXware writes the new order to pjsip.conf straight away, but
// Asterisk only reloads PJSIP config on PBXware's own 1-minute cycle
// (confirmed live 2026-09-26: reloads logged exactly 60s apart; one change
// applied after 3s, the next after 42s). Neither API can trigger a reload,
// so wait out a full cycle plus margin.
const trunkCodecSettle = 65 * time.Second

var errTrunkCodecBusy = errors.New("trunk codec busy")

// remoteBatchState tracks the remote batches running on one connected pair
// and the codec their trunks are ordered for.
type remoteBatchState struct {
	codec   string
	running int
}

// prepareRemoteBatch makes sure both trunks of a remote pair prefer codec
// before a batch using it starts. Why: when PBXware answers a call coming
// in over the trunk, it lists codecs in the trunk's own configured order,
// not the caller's. So with ulaw first, a G.729 call failed with 603
// (PBXware can't transcode G.729) and G.722/Opus calls were transcoded to
// ulaw on the trunk; with the batch's codec first, all of them pass
// through unchanged (confirmed live 2026-09-26).
//
// Both trunks are read first and only changed if their order isn't already
// right, so repeated batches with the same codec send no changes. The order
// applies to the whole trunk, so a batch with a different codec is refused
// while another remote batch on the same pair is still running — reordering
// then would make that batch's remaining calls fail.
//
// On success, the caller must call release exactly once when the batch has
// ended (or straight away if it started no calls). note is a line for the
// dashboard log, or "" if nothing needed changing.
func (a *app) prepareRemoteBatch(srv, peer *store.Server, codec string) (note string, release func(), err error) {
	key := srv.ID + "|" + peer.ID
	if !slices.Contains(pbxware.SwarmDialerCodecsList, codec) {
		return "", nil, fmt.Errorf("codec %q isn't allowed on the trunk", codec)
	}

	a.trunkMu.Lock()
	defer a.trunkMu.Unlock()

	st := a.remoteBatches[key]
	if st == nil {
		st = &remoteBatchState{}
		a.remoteBatches[key] = st
	}
	if st.running > 0 && st.codec != codec {
		return "", nil, fmt.Errorf("%w: a remote %s batch is still running on this pair, and the trunk codec order is shared by every call on it; wait for it to finish before starting a %s batch", errTrunkCodecBusy, st.codec, codec)
	}

	want := pbxware.TrunkCodecOrder(codec)
	var changed []string
	for _, s := range []*store.Server{srv, peer} {
		if s.TrunkID == 0 {
			continue
		}
		client := pbxware.NewClientV2(s.BaseURL, s.APIKeyV2)
		have, err := client.GetTrunkCodecs(s.TrunkID)
		if err != nil {
			return "", nil, fmt.Errorf("reading trunk codecs on %s: %w", s.Name, err)
		}
		if slices.Equal(have, want) {
			continue
		}
		if err := client.SetTrunkCodecs(s.TrunkID, want); err != nil {
			return "", nil, fmt.Errorf("setting trunk codec order on %s: %w", s.Name, err)
		}
		changed = append(changed, s.Name)
	}
	if len(changed) > 0 {
		n := a.startNotice("trunk:"+key, "remote", srv.Name+" → "+peer.Name,
			fmt.Sprintf("trunk codec order changed to %s first on %v; waiting up to %s for PBXware to apply it before dialing", codec, changed, trunkCodecSettle),
			fmt.Sprintf("trunk codec order is now %s first, dialing", codec))
		time.Sleep(trunkCodecSettle)
		n.finish(nil)
		note = fmt.Sprintf("trunk codec order set to %s first on %v", codec, changed)
	}

	st.codec = codec
	st.running++
	released := false
	return note, func() {
		a.trunkMu.Lock()
		defer a.trunkMu.Unlock()
		if !released {
			released = true
			st.running--
		}
	}, nil
}
