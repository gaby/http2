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
	go func() {
		defer close(done)
		ok := sc.enqueueFrameInternal(&FrameHeader{}, time.Second)
		if ok {
			t.Error("enqueueFrameInternal unexpectedly accepted frame")
		}
	}()

	time.Sleep(10 * time.Millisecond)
	close(sc.closer)

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("enqueueFrameInternal blocked after connection close")
	}
}

func TestCloseIdleConnReturnsWhenWriterBlocked(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		closer: make(chan struct{}),
	}
	sc.writer <- &FrameHeader{}

	start := time.Now()
	sc.closeIdleConn()
	elapsed := time.Since(start)

	if elapsed > 300*time.Millisecond {
		t.Fatalf("closeIdleConn took too long: %v", elapsed)
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
