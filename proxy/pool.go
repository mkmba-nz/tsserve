package proxy

import (
	"slices"
	"sync/atomic"
	"time"
)

// backend is one discovered endpoint that can serve an advertised service. It
// is immutable once created: pool mutation replaces the member slice rather
// than editing a member in place.
type backend struct {
	// key is the watcher-supplied registration key (a Docker container ID, or
	// taskArn#containerName in ECS).
	key string
	// addr is the backend's "host:port", used verbatim as the outbound URL host
	// and Host header. The scheme is a property of the advertised service.
	addr         string
	registeredAt time.Time
	origin       Origin
}

// backendPool is the ordered set of backends registered against one advertised
// service.
//
// Discovery mutates a pool at most once per task or container change, while the
// request path reads it on every request, so the member slice is copy-on-write:
// mutation happens under Manager.mu and publishes a fresh slice, and readers
// load the current slice without taking any lock. A reader therefore never
// waits on discovery — in particular never on a ListenService call or a
// listener teardown.
type backendPool struct {
	members atomic.Pointer[[]*backend]
	// next is the round-robin position, advanced once per request.
	next atomic.Uint64
}

func newBackendPool() *backendPool {
	p := &backendPool{}
	var empty []*backend
	p.members.Store(&empty)
	return p
}

// list returns the current members in pool order. The returned slice must not
// be modified: it is shared with every concurrent reader.
func (p *backendPool) list() []*backend {
	return *p.members.Load()
}

// add appends b and returns the resulting pool size. Callers hold Manager.mu.
func (p *backendPool) add(b *backend) int {
	next := append(slices.Clone(p.list()), b)
	p.members.Store(&next)
	return len(next)
}

// remove drops the member with the given registration key and returns the
// resulting pool size. Callers hold Manager.mu.
func (p *backendPool) remove(key string) int {
	next := slices.DeleteFunc(slices.Clone(p.list()), func(b *backend) bool { return b.key == key })
	p.members.Store(&next)
	return len(next)
}

// rotation returns the pool's current members together with the index of the
// member this call is assigned. It advances the round-robin position exactly
// once, so a request that walks the pool after a connection failure keeps the
// single position it was given. The members slice is empty when the pool is.
func (p *backendPool) rotation() ([]*backend, int) {
	members := p.list()
	if len(members) == 0 {
		return nil, 0
	}
	pos := p.next.Add(1) - 1
	return members, int(pos % uint64(len(members)))
}
