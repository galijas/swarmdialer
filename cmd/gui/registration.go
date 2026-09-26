package main

import (
	"sort"
	"time"
)

// registrationKeep is how long a finished registration stays in the live
// feed, so the dashboard reliably sees its final state before it's dropped.
const registrationKeep = 30 * time.Second

// registrationStatus is the live progress of one session's initial
// extension registration (see sessionFor) — shown in the dashboard's live
// status log, since a first dial on a fresh session otherwise sits silent
// while every extension registers.
type registrationStatus struct {
	ID    uint64 `json:"id"` // unique per attempt, so a retry shows as a new log line
	Key   string `json:"key"`
	Type  string `json:"type"` // "local" or "remote"
	Label string `json:"label"`
	// Sides is keyed by pool side: "" for local, "caller"/"callee" for remote.
	Sides map[string]*registrationSide `json:"sides"`
	Done  bool                         `json:"done"`
	Err   string                       `json:"error,omitempty"`
	// Message, if set, makes this a plain status notice (e.g. waiting for a
	// trunk codec change to apply) shown as-is instead of registration
	// counts; DoneMessage replaces it once finished.
	Message     string `json:"message,omitempty"`
	DoneMessage string `json:"done_message,omitempty"`
	finishedAt  time.Time
}

type registrationSide struct {
	Registered int `json:"registered"`
	Failed     int `json:"failed"`
	Total      int `json:"total"`
}

// registrationHandle is what sessionFor uses to report into one
// registrationStatus.
type registrationHandle struct {
	a      *app
	status *registrationStatus
}

func (a *app) startRegistration(key, typ, label string) *registrationHandle {
	a.regMu.Lock()
	defer a.regMu.Unlock()
	a.nextRegID++
	st := &registrationStatus{ID: a.nextRegID, Key: key, Type: typ, Label: label, Sides: map[string]*registrationSide{}}
	a.registrations[key] = st
	return &registrationHandle{a: a, status: st}
}

// startNotice adds a plain status notice to the live feed (see
// registrationStatus.Message); finish it with finishNotice.
func (a *app) startNotice(key, typ, label, message, doneMessage string) *registrationHandle {
	h := a.startRegistration(key, typ, label)
	a.regMu.Lock()
	h.status.Message, h.status.DoneMessage = message, doneMessage
	a.regMu.Unlock()
	return h
}

// update matches orchestrator.RegisterProgressFunc.
func (h *registrationHandle) update(side string, registered, failed, total int) {
	h.a.regMu.Lock()
	defer h.a.regMu.Unlock()
	sd := h.status.Sides[side]
	if sd == nil {
		sd = &registrationSide{}
		h.status.Sides[side] = sd
	}
	// Progress callbacks race each other; never let a late, stale one move
	// the counters backwards.
	if registered+failed >= sd.Registered+sd.Failed {
		sd.Registered, sd.Failed, sd.Total = registered, failed, total
	}
}

func (h *registrationHandle) finish(err error) {
	h.a.regMu.Lock()
	defer h.a.regMu.Unlock()
	h.status.Done = true
	if err != nil {
		h.status.Err = err.Error()
	}
	h.status.finishedAt = time.Now()
}

// registrationSnapshot is a JSON-safe copy of a registrationStatus.
type registrationSnapshot struct {
	ID    uint64                      `json:"id"`
	Key   string                      `json:"key"`
	Type  string                      `json:"type"`
	Label string                      `json:"label"`
	Sides map[string]registrationSide `json:"sides"`
	Done  bool                        `json:"done"`
	Err   string                      `json:"error,omitempty"`

	Message     string `json:"message,omitempty"`
	DoneMessage string `json:"done_message,omitempty"`
}

// registrationSnapshots returns every in-progress registration plus ones
// finished within registrationKeep, dropping older ones.
func (a *app) registrationSnapshots() []registrationSnapshot {
	a.regMu.Lock()
	defer a.regMu.Unlock()
	out := make([]registrationSnapshot, 0, len(a.registrations))
	for key, st := range a.registrations {
		if st.Done && time.Since(st.finishedAt) > registrationKeep {
			delete(a.registrations, key)
			continue
		}
		sides := make(map[string]registrationSide, len(st.Sides))
		for k, v := range st.Sides {
			sides[k] = *v
		}
		out = append(out, registrationSnapshot{ID: st.ID, Key: st.Key, Type: st.Type, Label: st.Label, Sides: sides, Done: st.Done, Err: st.Err, Message: st.Message, DoneMessage: st.DoneMessage})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
