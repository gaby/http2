package http2

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"testing"

	"github.com/dgrr/http2/http2utils"
)

func TestFrameAndHeaderFieldHelpers(t *testing.T) {
	// FrameType String fallback and flag helpers
	if FrameType(42).String() != "42" {
		t.Fatalf("unexpected string for unknown frame type")
	}

	var flags FrameFlags = FlagEndHeaders
	flags = flags.Add(FlagPadded)
	if got := flags.Del(FlagEndHeaders); !got.Has(FlagPadded) || got.Has(FlagEndHeaders) {
		t.Fatalf("del flag mismatch: %v", got)
	}

	fr := AcquireFrameHeader()
	fr.SetBody(AcquireFrame(FrameData))
	fr.SetStream(123)
	fr.setPayload([]byte{1, 2, 3})
	fr.length = len(fr.payload)
	if fr.Len() != 3 {
		t.Fatalf("Len mismatch: %d", fr.Len())
	}
	if fr.MaxLen() == 0 {
		t.Fatalf("unexpected MaxLen: %d", fr.MaxLen())
	}
	ReleaseFrameHeader(fr)

	hf := AcquireHeaderField()
	hf.Set("Key", "Value")
	if hf.IsSensible() {
		t.Fatalf("unexpected sensible flag")
	}
	if got := hf.Key(); got != "Key" {
		t.Fatalf("unexpected key: %s", got)
	}
	if got := hf.Value(); got != "Value" {
		t.Fatalf("unexpected value: %s", got)
	}
	if hf.Empty() {
		t.Fatalf("header should not be empty")
	}
	ReleaseHeaderField(hf)
}

func TestUtilityConversions(t *testing.T) {
	b := make([]byte, 3)
	http2utils.Uint24ToBytes(b, 0x010203)
	if got := http2utils.BytesToUint24(b); got != 0x010203 {
		t.Fatalf("unexpected uint24: %x", got)
	}

	b4 := make([]byte, 4)
	http2utils.Uint32ToBytes(b4, 0x0a0b0c0d)
	if got := http2utils.BytesToUint32(b4); got != 0x0a0b0c0d {
		t.Fatalf("unexpected uint32: %x", got)
	}

	if !http2utils.EqualsFold([]byte("HeLlO"), []byte("hello")) {
		t.Fatalf("expected fold equality")
	}

	resized := http2utils.Resize(make([]byte, 0, 2), 5)
	if len(resized) != 5 {
		t.Fatalf("resize failed: %d", len(resized))
	}

	padded := http2utils.AddPadding([]byte("abc"))
	if len(padded) <= len("abc")+1 {
		t.Fatalf("padding not added")
	}
	stripped, err := http2utils.CutPadding(padded, len(padded))
	if err != nil {
		t.Fatalf("cut padding: %v", err)
	}
	if !bytes.Equal(stripped, []byte("abc")) {
		t.Fatalf("unexpected stripped data: %q", stripped)
	}
}

func TestErrorHelpers(t *testing.T) {
	if ErrorCode(99).String() != "Unknown" {
		t.Fatalf("unexpected string for unknown code")
	}

	err := NewGoAwayError(InternalError, "debug")
	if !err.Is(InternalError) {
		t.Fatalf("expected errors.Is to match code")
	}
	if err.Code() != InternalError {
		t.Fatalf("unexpected code: %v", err.Code())
	}
	if err.Debug() != "debug" {
		t.Fatalf("unexpected debug: %s", err.Debug())
	}
	if err.Error() == "" {
		t.Fatalf("expected formatted error")
	}
}

func TestConfigureDialerSetsDefaults(t *testing.T) {
	d := &Dialer{Addr: "example.com:443"}
	configureDialer(d)
	if d.TLSConfig == nil {
		t.Fatalf("TLSConfig not set")
	}
	if d.TLSConfig.ServerName != "example.com" {
		t.Fatalf("unexpected server name: %s", d.TLSConfig.ServerName)
	}
	found := false
	for _, p := range d.TLSConfig.NextProtos {
		if p == "h2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("h2 not injected in NextProtos")
	}

	// ServerName should not be overridden when already present
	cfg := &tls.Config{ServerName: "custom"}
	d2 := &Dialer{Addr: "ignored:443", TLSConfig: cfg}
	configureDialer(d2)
	if d2.TLSConfig.ServerName != "custom" {
		t.Fatalf("existing server name modified")
	}
}

func TestClientOptionsSanitize(t *testing.T) {
	opts := ClientOpts{}
	opts.sanitize()
	if opts.MaxResponseTime != DefaultMaxResponseTime {
		t.Fatalf("default MaxResponseTime not applied")
	}
	if opts.PingInterval != DefaultPingInterval {
		t.Fatalf("default PingInterval not applied")
	}
}

func TestCtxResolveNonBlocking(t *testing.T) {
	ch := make(chan error, 1)
	ctx := &Ctx{Err: ch}
	ctx.resolve(io.EOF)
	ctx.resolve(nil) // should not panic or block
	if err := <-ch; err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnCancelErrors(t *testing.T) {
	c := &Conn{}
	ctx := &Ctx{Err: make(chan error, 1)}
	if err := c.Cancel(ctx); !errors.Is(err, ErrStreamNotReady) {
		t.Fatalf("expected ErrStreamNotReady, got %v", err)
	}
}
