package http2

import (
	"testing"

	"github.com/valyala/fasthttp"
)

func BenchmarkHPACKEncodeTypicalResponse(b *testing.B) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)

	res := &fasthttp.Response{}
	res.SetStatusCode(200)
	res.Header.Set("Content-Type", "application/json")
	res.Header.Set("Content-Length", "1234")
	res.Header.Set("Server", "fasthttp")
	res.Header.Set("X-Request-Id", "abc-123-def")

	h := AcquireFrame(FrameHeaders).(*Headers)
	defer ReleaseFrame(h)

	b.ReportAllocs()
	for b.Loop() {
		h.Reset()
		fasthttpResponseHeaders(h, hp, res)
	}
}

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
