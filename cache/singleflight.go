package cache

import "sync"

// flightGroup deduplicates concurrent calls keyed by string: the first
// caller for a key runs the function while the rest wait, then all share
// its result. It is a minimal stand-in for golang.org/x/sync/singleflight,
// kept in-package to stay dependency-free — without that package's
// duplicate-call accounting.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	wg    sync.WaitGroup
	val   any
	err   error
	panic any // a value recovered from fn, re-raised in every caller
}

func newFlightGroup() *flightGroup {
	return &flightGroup{calls: make(map[string]*flightCall)}
}

// Do runs fn unless a call for key is already in flight, in which case it
// waits for that call and shares its result. If fn panics, the panic is
// re-raised in the leader and in every waiter (rather than leaving
// waiters to read an unset result) — a load panic stays a panic, surfaced
// consistently, matching x/sync/singleflight's semantics.
func (g *flightGroup) Do(key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if call, ok := g.calls[key]; ok {
		g.mu.Unlock()
		call.wg.Wait()
		if call.panic != nil {
			panic(call.panic)
		}
		return call.val, call.err
	}
	call := &flightCall{}
	call.wg.Add(1)
	g.calls[key] = call
	g.mu.Unlock()

	g.exec(key, call, fn)

	if call.panic != nil {
		panic(call.panic)
	}
	return call.val, call.err
}

// exec runs fn for the leader, recovering any panic so it can be re-raised
// by Do in all callers. The in-flight entry is always cleared, so a
// panicking load can neither deadlock waiters nor wedge the key.
func (g *flightGroup) exec(key string, call *flightCall, fn func() (any, error)) {
	defer func() {
		if r := recover(); r != nil {
			call.panic = r
		}
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		call.wg.Done()
	}()
	call.val, call.err = fn()
}
