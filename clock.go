package http2

import "time"

// Clock defines the minimal interface required to create and control timers.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, fn func()) Timer
	NewTimer(d time.Duration) Timer
}

// Timer defines the minimal timer contract used throughout the package.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}

func (realClock) AfterFunc(d time.Duration, fn func()) Timer {
	return &realTimer{t: time.AfterFunc(d, fn)}
}

func (realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{t: time.NewTimer(d)}
}

type realTimer struct {
	t *time.Timer
}

func (rt *realTimer) C() <-chan time.Time {
	return rt.t.C
}

func (rt *realTimer) Stop() bool {
	return rt.t.Stop()
}

func (rt *realTimer) Reset(d time.Duration) bool {
	return rt.t.Reset(d)
}
