package http2

import (
	"testing"
	"time"
)

func BenchmarkServerConnEnqueueFrameInternalFastPath(b *testing.B) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		closer: make(chan struct{}),
	}
	fr := &FrameHeader{}

	b.ReportAllocs()
	for b.Loop() {
		if !sc.enqueueFrameInternal(fr, time.Second) {
			b.Fatal("enqueueFrameInternal returned false")
		}
		<-sc.writer
	}
}

func BenchmarkServerConnEnqueueFrameInternalFullChannel(b *testing.B) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		closer: make(chan struct{}),
	}
	sc.writer <- &FrameHeader{}
	fr := &FrameHeader{}

	b.ReportAllocs()
	for b.Loop() {
		_ = sc.enqueueFrameInternal(fr, time.Nanosecond)
	}
}
