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

func BenchmarkHPACKDecodeTypicalRequest(b *testing.B) {
	// Encode a typical request header set, then benchmark decoding.
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)

	hf := AcquireHeaderField()

	var encoded []byte

	hf.SetBytes([]byte(":method"), []byte("GET"))
	encoded = hp.AppendHeader(encoded, hf, true)

	hf.SetBytes([]byte(":path"), []byte("/api/v1/users"))
	encoded = hp.AppendHeader(encoded, hf, true)

	hf.SetBytes([]byte(":scheme"), []byte("https"))
	encoded = hp.AppendHeader(encoded, hf, true)

	hf.SetBytes([]byte(":authority"), []byte("example.com"))
	encoded = hp.AppendHeader(encoded, hf, true)

	hf.SetBytes([]byte("accept"), []byte("application/json"))
	encoded = hp.AppendHeader(encoded, hf, false)

	hf.SetBytes([]byte("user-agent"), []byte("fasthttp/1.0"))
	encoded = hp.AppendHeader(encoded, hf, false)

	ReleaseHeaderField(hf)

	// Now benchmark decoding
	decHP := AcquireHPACK()
	defer ReleaseHPACK(decHP)
	decHF := AcquireHeaderField()
	defer ReleaseHeaderField(decHF)

	b.ReportAllocs()
	for b.Loop() {
		data := encoded
		var err error
		for len(data) > 0 {
			data, err = decHP.Next(decHF, data)
			if err != nil {
				b.Fatal(err)
			}
		}
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
