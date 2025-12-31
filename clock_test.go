package http2

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// FakeClock is a controllable clock implementation for tests.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]struct{}
}

type fakeTimer struct {
	clock *FakeClock
	c     chan time.Time

	when    time.Time
	fn      func()
	stopped bool
	fired   bool
}

// NewFakeClock returns a FakeClock starting at the provided instant.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{
		now:    start,
		timers: make(map[*fakeTimer]struct{}),
	}
}

func (fc *FakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	return fc.now
}

func (fc *FakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	return fc.newTimer(d, fn)
}

func (fc *FakeClock) NewTimer(d time.Duration) Timer {
	return fc.newTimer(d, nil)
}

func (fc *FakeClock) newTimer(d time.Duration, fn func()) Timer {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	t := &fakeTimer{
		clock: fc,
		c:     make(chan time.Time, 1),
		fn:    fn,
	}
	t.when = fc.now.Add(d)
	t.stopped = false
	t.fired = false
	fc.timers[t] = struct{}{}

	// Fire immediately if the deadline is now or in the past.
	if !t.when.After(fc.now) {
		go t.fire()
	}

	return t
}

// Advance moves the fake clock forward and triggers any expired timers.
func (fc *FakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	fc.now = fc.now.Add(d)

	var ready []*fakeTimer
	for t := range fc.timers {
		if t.stopped || t.fired {
			continue
		}
		if !t.when.After(fc.now) {
			ready = append(ready, t)
		}
	}
	fc.mu.Unlock()

	for _, t := range ready {
		t.fire()
	}
}

// PendingTimers returns the number of timers managed by the fake clock.
func (fc *FakeClock) PendingTimers() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	return len(fc.timers)
}

// NextFireIn returns the duration until the soonest scheduled timer fires.
// A zero duration indicates either no timers or a timer that is already due.
func (fc *FakeClock) NextFireIn() time.Duration {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	var minDeadline *time.Time
	for t := range fc.timers {
		if t.stopped || t.fired {
			continue
		}

		if minDeadline == nil || t.when.Before(*minDeadline) {
			min := t.when
			minDeadline = &min
		}
	}

	if minDeadline == nil {
		return 0
	}

	return minDeadline.Sub(fc.now)
}

func TestFakeClockAdvancesTimers(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))

	timer := clock.NewTimer(time.Second)
	clock.Advance(time.Second)

	select {
	case <-timer.C():
	default:
		t.Fatalf("timer did not fire after advancing")
	}
}

func TestFakeClockAfterFunc(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))

	fired := make(chan struct{}, 1)
	clock.AfterFunc(time.Second, func() {
		fired <- struct{}{}
	})

	clock.Advance(time.Second)

	require.Eventually(t, func() bool {
		select {
		case <-fired:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond*10, "after func did not execute after advancing")
}

func (t *fakeTimer) C() <-chan time.Time {
	return t.c
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	active := !t.stopped && !t.fired
	t.stopped = true
	delete(t.clock.timers, t)

	return active
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	wasActive := !t.stopped && !t.fired
	t.when = t.clock.now.Add(d)
	t.stopped = false
	t.fired = false
	t.clock.timers[t] = struct{}{}

	if !t.when.After(t.clock.now) {
		go t.fire()
	}

	return wasActive
}

func (t *fakeTimer) fire() {
	t.clock.mu.Lock()
	if t.stopped || t.fired {
		t.clock.mu.Unlock()
		return
	}

	t.fired = true
	delete(t.clock.timers, t)
	now := t.clock.now
	t.clock.mu.Unlock()

	if t.fn != nil {
		go t.fn()
		return
	}

	select {
	case t.c <- now:
	default:
	}
}
