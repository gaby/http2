package http2

import "testing"

func BenchmarkHeadersSerializePriority(b *testing.B) {
	h := &Headers{}
	raw := []byte("abcdefghijklmnopqrstuvwxyz0123456789")

	frh := &FrameHeader{}
	b.ReportAllocs()

	for b.Loop() {
		h.hasPadding = false
		h.stream = 123
		h.weight = 16
		h.endStream = false
		h.endHeaders = true
		h.priority = true
		h.rawHeaders = raw
		frh.Reset()
		h.Serialize(frh)
	}
}

func BenchmarkHeadersSerializeNoPriority(b *testing.B) {
	h := &Headers{}
	raw := []byte("abcdefghijklmnopqrstuvwxyz0123456789")

	frh := &FrameHeader{}
	b.ReportAllocs()

	for b.Loop() {
		h.hasPadding = false
		h.stream = 0
		h.weight = 0
		h.endStream = false
		h.endHeaders = true
		h.priority = false
		h.rawHeaders = raw
		frh.Reset()
		h.Serialize(frh)
	}
}
