package http2

import (
	"testing"
	"time"
)

func TestEnqueueFrameInternalReturnsOnClose(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		closer: make(chan struct{}),
	}
	sc.writer <- &FrameHeader{}

	done := make(chan struct{})
	entered := make(chan struct{})
	go func() {
		defer close(done)
		close(entered)
		ok := sc.enqueueFrameInternal(&FrameHeader{}, time.Second)
		if ok {
			t.Error("enqueueFrameInternal unexpectedly accepted frame")
		}
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for enqueue goroutine to start")
	}
	close(sc.closer)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueFrameInternal blocked after connection close")
	}
}

func TestCloseIdleConnReturnsWhenWriterBlocked(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		closer: make(chan struct{}),
	}
	sc.writer <- &FrameHeader{}

	// closeIdleConn must return even though the writer channel is full and the
	// closer is not yet closed. Use a generous deadlock detector rather than a
	// tight wall-clock bound, which would flake under load on CI.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.closeIdleConn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closeIdleConn did not return while writer was blocked")
	}

	if !sc.closing.Load() {
		t.Fatal("connection was not marked as closing")
	}
	select {
	case <-sc.closer:
	default:
		t.Fatal("closeIdleConn did not close closer channel")
	}
}
