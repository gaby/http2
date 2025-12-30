package http2

import (
	"bytes"
	"crypto/tls"
	"io"
	"testing"

	"github.com/dgrr/http2/http2utils"
	"github.com/stretchr/testify/require"
)

func TestFrameAndHeaderFieldHelpers(t *testing.T) {
	// FrameType String fallback and flag helpers
	require.Equal(t, "42", FrameType(42).String(), "unexpected string for unknown frame type")

	var flags FrameFlags = FlagEndHeaders
	flags = flags.Add(FlagPadded)
	gotFlags := flags.Del(FlagEndHeaders)
	require.True(t, gotFlags.Has(FlagPadded), "padded flag should remain after deletion")
	require.False(t, gotFlags.Has(FlagEndHeaders), "end headers flag should be removed")

	fr := AcquireFrameHeader()
	fr.SetBody(AcquireFrame(FrameData))
	fr.SetStream(123)
	fr.setPayload([]byte{1, 2, 3})
	fr.length = len(fr.payload)
	require.Equal(t, 3, fr.Len())
	require.NotZero(t, fr.MaxLen())
	ReleaseFrameHeader(fr)

	hf := AcquireHeaderField()
	hf.Set("Key", "Value")
	require.False(t, hf.IsSensible(), "unexpected sensible flag")
	require.Equal(t, "Key", hf.Key())
	require.Equal(t, "Value", hf.Value())
	require.False(t, hf.Empty(), "header should not be empty")
	ReleaseHeaderField(hf)
}

func TestUtilityConversions(t *testing.T) {
	b := make([]byte, 3)
	http2utils.Uint24ToBytes(b, 0x010203)
	require.Equal(t, uint32(0x010203), http2utils.BytesToUint24(b))

	b4 := make([]byte, 4)
	http2utils.Uint32ToBytes(b4, 0x0a0b0c0d)
	require.Equal(t, uint32(0x0a0b0c0d), http2utils.BytesToUint32(b4))

	require.True(t, http2utils.EqualsFold([]byte("HeLlO"), []byte("hello")), "expected fold equality")

	resized := http2utils.Resize(make([]byte, 0, 2), 5)
	require.Len(t, resized, 5)

	padded := http2utils.AddPadding([]byte("abc"))
	require.Greater(t, len(padded), len("abc")+1, "padding not added")
	stripped, err := http2utils.CutPadding(padded, len(padded))
	require.NoError(t, err)
	require.True(t, bytes.Equal(stripped, []byte("abc")), "unexpected stripped data: %q", stripped)
}

func TestErrorHelpers(t *testing.T) {
	require.Equal(t, "Unknown", ErrorCode(99).String(), "unexpected string for unknown code")

	err := NewGoAwayError(InternalError, "debug")
	require.True(t, err.Is(InternalError), "expected errors.Is to match code")
	require.Equal(t, InternalError, err.Code())
	require.Equal(t, "debug", err.Debug())
	require.NotEmpty(t, err.Error())

	resetErr := NewResetStreamError(EnhanceYourCalm, "boom")
	require.True(t, resetErr.Is(EnhanceYourCalm))
	require.Equal(t, EnhanceYourCalm, resetErr.Code())
	require.Contains(t, resetErr.Error(), "boom")
}

func TestConfigureDialerSetsDefaults(t *testing.T) {
	d := &Dialer{Addr: "example.com:443"}
	configureDialer(d)
	require.NotNil(t, d.TLSConfig, "TLSConfig not set")
	require.Equal(t, "example.com", d.TLSConfig.ServerName)
	found := false
	for _, p := range d.TLSConfig.NextProtos {
		if p == "h2" {
			found = true
		}
	}
	require.True(t, found, "h2 not injected in NextProtos")

	// ServerName should not be overridden when already present
	cfg := &tls.Config{ServerName: "custom"}
	d2 := &Dialer{Addr: "ignored:443", TLSConfig: cfg}
	configureDialer(d2)
	require.Equal(t, "custom", d2.TLSConfig.ServerName, "existing server name modified")
}

func TestClientOptionsSanitize(t *testing.T) {
	opts := ClientOpts{}
	opts.sanitize()
	require.Equal(t, DefaultMaxResponseTime, opts.MaxResponseTime, "default MaxResponseTime not applied")
	require.Equal(t, DefaultPingInterval, opts.PingInterval, "default PingInterval not applied")
}

func TestCtxResolveNonBlocking(t *testing.T) {
	ch := make(chan error, 1)
	ctx := &Ctx{Err: ch}
	ctx.resolve(io.EOF)
	ctx.resolve(nil) // should not panic or block
	err := <-ch
	require.ErrorIs(t, err, io.EOF)
}

func TestConnCancelErrors(t *testing.T) {
	c := &Conn{}
	ctx := &Ctx{Err: make(chan error, 1)}
	err := c.Cancel(ctx)
	require.ErrorIs(t, err, ErrStreamNotReady)
}
