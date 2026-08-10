package ecs

import (
	"sync"
	"time"
)

// ReaderStatus is a point-in-time, read-only view of one configured ECS reader
// (an account/cluster credential context that a Watcher polls). It is what the
// status page renders in the "readers" table.
type ReaderStatus struct {
	// Name is the reader's display identity: an explicit account name, the AWS
	// account ID resolved via STS, or a positional fallback when neither is
	// available.
	Name string
	// Account is the AWS account ID resolved via STS GetCallerIdentity, or empty
	// when it could not be resolved (e.g. the cross-account role is not
	// assumable, or single-account mode where no STS call is made).
	Account string
	Cluster string
	Region  string
	// PollInterval is how often this reader's watcher polls ECS.
	PollInterval time.Duration
	// LastPollStart is when the most recent poll cycle began (zero if none has
	// started yet).
	LastPollStart time.Time
	// LastPollOK is when the most recent poll cycle last succeeded (zero if it
	// has never succeeded).
	LastPollOK time.Time
	// LastError is the message from the most recent failed poll (or startup
	// identity resolution), or empty when the last poll succeeded.
	LastError string
	// Polls counts completed poll cycles (successful or failed).
	Polls uint64
	// Healthy is true once a poll has succeeded and the most recent poll did not
	// fail. A reader that has never polled, or whose last poll errored, is not
	// healthy.
	Healthy bool
}

// readerRecord is the mutable state behind a ReaderStatus, guarded by the
// owning Registry's mutex.
type readerRecord struct {
	status ReaderStatus
}

// Registry holds live status for every configured ECS reader so the status
// page can display the fleet — including readers whose credentials never
// resolved. It is safe for concurrent use.
type Registry struct {
	mu    sync.Mutex
	order []*readerRecord
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// Add records a reader's initial (config-time) status and returns a Reader
// handle its Watcher uses to report poll outcomes. Readers appear in the
// snapshot in the order they are added.
func (r *Registry) Add(initial ReaderStatus) *Reader {
	rec := &readerRecord{status: initial}
	r.mu.Lock()
	r.order = append(r.order, rec)
	r.mu.Unlock()
	return &Reader{reg: r, rec: rec}
}

// Snapshot returns a copy of every reader's current status, in insertion order.
func (r *Registry) Snapshot() []ReaderStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ReaderStatus, len(r.order))
	for i, rec := range r.order {
		out[i] = rec.status
	}
	return out
}

// Reader is a Watcher's handle to its own entry in a Registry. A nil *Reader is
// valid and its methods are no-ops, so the single-account and test paths can
// leave it unset.
type Reader struct {
	reg *Registry
	rec *readerRecord
}

// PollStarted marks the beginning of a poll cycle.
func (h *Reader) PollStarted(now time.Time) {
	if h == nil {
		return
	}
	h.reg.mu.Lock()
	h.rec.status.LastPollStart = now
	h.reg.mu.Unlock()
}

// PollSucceeded marks a poll cycle as completed without error, clearing any
// previous error and marking the reader healthy.
func (h *Reader) PollSucceeded(now time.Time) {
	if h == nil {
		return
	}
	h.reg.mu.Lock()
	h.rec.status.LastPollOK = now
	h.rec.status.LastError = ""
	h.rec.status.Polls++
	h.rec.status.Healthy = true
	h.reg.mu.Unlock()
}

// PollFailed records a failed poll cycle. The reader is marked unhealthy but its
// last-successful-poll timestamp is preserved.
func (h *Reader) PollFailed(now time.Time, err error) {
	if h == nil {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	h.reg.mu.Lock()
	h.rec.status.LastError = msg
	h.rec.status.Polls++
	h.rec.status.Healthy = false
	h.reg.mu.Unlock()
}

// NoteError records a startup-time problem (e.g. credentials that will not
// resolve, or an unassumable role) without counting it as a poll cycle. The
// reader is marked unhealthy so the status page surfaces the failure even
// before the first poll runs.
func (h *Reader) NoteError(err error) {
	if h == nil {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	h.reg.mu.Lock()
	h.rec.status.LastError = msg
	h.rec.status.Healthy = false
	h.reg.mu.Unlock()
}

// SetAccount records the AWS account ID resolved for this reader (via STS).
func (h *Reader) SetAccount(account string) {
	if h == nil {
		return
	}
	h.reg.mu.Lock()
	h.rec.status.Account = account
	h.reg.mu.Unlock()
}
