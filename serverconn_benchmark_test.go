package http2

import (
	"testing"
	"time"
)

func BenchmarkIsValidHTTP2HeaderNameShort(b *testing.B) {
	name := []byte("content-type")
	b.ReportAllocs()
	for b.Loop() {
		isValidHTTP2HeaderName(name)
	}
}

func BenchmarkIsValidHTTP2HeaderNameLong(b *testing.B) {
	name := []byte("x-custom-very-long-header-name-for-benchmarking")
	b.ReportAllocs()
	for b.Loop() {
		isValidHTTP2HeaderName(name)
	}
}

func BenchmarkIsValidHTTP2HeaderNamePseudo(b *testing.B) {
	name := []byte(":authority")
	b.ReportAllocs()
	for b.Loop() {
		isValidHTTP2HeaderName(name)
	}
}

func BenchmarkIsForbiddenHeaderHit(b *testing.B) {
	key := []byte("connection")
	value := []byte("close")
	b.ReportAllocs()
	for b.Loop() {
		isForbiddenHeader(key, value)
	}
}

func BenchmarkIsForbiddenHeaderMiss(b *testing.B) {
	key := []byte("content-type")
	value := []byte("application/json")
	b.ReportAllocs()
	for b.Loop() {
		isForbiddenHeader(key, value)
	}
}

func BenchmarkIsForbiddenHeaderTE(b *testing.B) {
	key := []byte("te")
	value := []byte("trailers")
	b.ReportAllocs()
	for b.Loop() {
		isForbiddenHeader(key, value)
	}
}

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
