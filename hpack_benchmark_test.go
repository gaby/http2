package http2

import "testing"

func BenchmarkHPACKAppendHeaderStaticFullMatch(b *testing.B) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.DisableCompression = false

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)
	hf.SetBytes([]byte(":method"), []byte("GET"))

	dst := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		dst = hp.AppendHeader(dst[:0], hf, false)
	}
}

func BenchmarkHPACKAppendHeaderStaticKeyMatch(b *testing.B) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.DisableCompression = false

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)
	hf.SetBytes([]byte(":path"), []byte("/custom"))

	dst := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		dst = hp.AppendHeader(dst[:0], hf, false)
	}
}
