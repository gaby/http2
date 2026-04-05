package http2

import (
	"io"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---- PING rate limiting --------------------------------------------------

func newTestServerConnWithWriter(bufSize int) *serverConn {
	sc := &serverConn{
		logger: log.New(io.Discard, "", 0),
		writer: make(chan *FrameHeader, bufSize),
	}
	sc.dec.Reset()
	sc.st.Reset()
	return sc
}

// TestHandlePingRateLimitAllowsNormal verifies that up to pingRateMax PINGs
// per window are answered with ACKs.
func TestHandlePingRateLimitAllowsNormal(t *testing.T) {
	sc := newTestServerConnWithWriter(pingRateMax + 4)

	for i := range pingRateMax {
		ping := &Ping{}
		sc.handlePing(ping)
		fr := <-sc.writer
		require.Equal(t, FramePing, fr.Type())
		require.True(t, fr.Body().(*Ping).IsAck(), "ping %d should be acked", i)
		ReleaseFrameHeader(fr)
	}
}

// TestHandlePingRateLimitSilentlyDropsExcess verifies that PINGs beyond the
// per-window rate limit are silently dropped (no frame queued, no GOAWAY yet).
func TestHandlePingRateLimitSilentlyDropsExcess(t *testing.T) {
	sc := newTestServerConnWithWriter(pingRateMax + 4)

	// Fill the window quota.
	for range pingRateMax {
		ping := &Ping{}
		sc.handlePing(ping)
		fr := <-sc.writer
		ReleaseFrameHeader(fr)
	}

	// One extra ping in the same window should be dropped silently.
	ping := &Ping{}
	sc.handlePing(ping)
	require.Zero(t, len(sc.writer), "excess ping should be silently dropped")
}

// TestHandlePingRateLimitGoAwayAfterViolations verifies that after
// pingMaxViolation consecutive over-limit windows the server sends GOAWAY.
func TestHandlePingRateLimitGoAwayAfterViolations(t *testing.T) {
	sc := newTestServerConnWithWriter(128)

	// Build up pingMaxViolation consecutive violated windows. Each iteration:
	//   1. expire the previous window so the window-transition logic runs,
	//   2. send pingRateMax+1 pings so sc.pingCount ends > pingRateMax.
	for range pingMaxViolation {
		// Expire the current window before starting fresh.
		sc.pingMu.Lock()
		sc.pingWindowStart = time.Now().Add(-2 * pingRateWindow)
		sc.pingMu.Unlock()

		// Send enough pings to exceed the rate, including one over the limit.
		for range pingRateMax + 1 {
			ping := &Ping{}
			sc.handlePing(ping)
			// Drain any queued ACKs without blocking.
			for len(sc.writer) > 0 {
				fr := <-sc.writer
				ReleaseFrameHeader(fr)
			}
		}
	}

	// After pingMaxViolation violated windows, any subsequent ping should
	// trigger a GOAWAY because violations >= pingMaxViolation.
	// Expire the window so the previous window's verdict is evaluated.
	sc.pingMu.Lock()
	sc.pingWindowStart = time.Now().Add(-2 * pingRateWindow)
	sc.pingMu.Unlock()

	// Send one ping to trigger the window-transition evaluation.
	ping := &Ping{}
	sc.handlePing(ping)

	// Drain any non-GOAWAY frames.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(sc.writer) == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		fr := <-sc.writer
		if fr.Type() == FrameGoAway {
			ga := fr.Body().(*GoAway)
			require.Equal(t, EnhanceYourCalm, ga.Code())
			ReleaseFrameHeader(fr)
			return
		}
		ReleaseFrameHeader(fr)
	}
	t.Fatal("expected GOAWAY with EnhanceYourCalm but none received")
}

// ---- RST_STREAM flood detection ------------------------------------------

// TestHandleFrameRSTFloodDetection verifies that exceeding rstRateMax RST
// frames in one window returns a GOAWAY error.
func TestHandleFrameRSTFloodDetection(t *testing.T) {
	sc := newTestServerConnWithWriter(4)
	sc.rstMu.Lock()
	sc.rstCount = rstRateMax // pre-fill bucket to threshold
	sc.rstWindowStart = time.Now()
	sc.rstMu.Unlock()

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	rst := AcquireFrame(FrameResetStream).(*RstStream)
	rst.SetCode(StreamCanceled)
	fr.SetBody(rst)

	err := sc.handleFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, EnhanceYourCalm, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)
	ReleaseFrameHeader(fr)
}

// TestHandleFrameRSTWindowResets verifies that RST tracking resets after the
// window duration and does not produce false positives.
func TestHandleFrameRSTWindowResets(t *testing.T) {
	sc := newTestServerConnWithWriter(4)
	// Pre-fill bucket at threshold but with an expired window start.
	sc.rstMu.Lock()
	sc.rstCount = rstRateMax
	sc.rstWindowStart = time.Now().Add(-2 * rstRateWindow)
	sc.rstMu.Unlock()

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	rst := AcquireFrame(FrameResetStream).(*RstStream)
	rst.SetCode(StreamCanceled)
	fr.SetBody(rst)

	err := sc.handleFrame(strm, fr)
	// Window expired, so the count should have reset — no flood error.
	require.NoError(t, err)
	ReleaseFrameHeader(fr)
}

// ---- CONTINUATION flood (maxContinuationFrames) -------------------------

// TestHandleHeaderFrameContinuationFlood verifies that a stream that has
// already accumulated maxContinuationFrames header blocks is rejected.
func TestHandleHeaderFrameContinuationFlood(t *testing.T) {
	sc := newTestServerConnWithWriter(4)

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)
	strm.headerBlockNum = maxContinuationFrames // already at limit

	fr := buildHeadersFrame(t, 1, [][2]string{
		{":method", "GET"},
		{":path", "/"},
		{":scheme", "https"},
		{":authority", "example.com"},
	})

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)
	ReleaseFrameHeader(fr)
}

// ---- Header list size enforcement ----------------------------------------

// TestHandleHeaderFrameMaxHeaderListSize verifies that a header block whose
// decoded size exceeds SETTINGS_MAX_HEADER_LIST_SIZE returns a compression error.
func TestHandleHeaderFrameMaxHeaderListSize(t *testing.T) {
	sc := newTestServerConnWithWriter(4)
	// Set a very small limit so a single header overflows it.
	sc.st.SetMaxHeaderListSize(10)

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)

	// Build a headers frame with fields large enough to exceed 10 bytes.
	fr := buildHeadersFrame(t, 1, [][2]string{
		{":method", "GET"},
		{":path", "/"},
		{":scheme", "https"},
		{":authority", "example.com"},
	})

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, CompressionError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)
	ReleaseFrameHeader(fr)
}

// TestHandleHeaderFrameHeaderListSizeAllowed verifies that a header block
// within the configured limit is accepted normally.
func TestHandleHeaderFrameHeaderListSizeAllowed(t *testing.T) {
	sc := newTestServerConnWithWriter(4)
	// Use a large limit so everything fits.
	sc.st.SetMaxHeaderListSize(defaultMaxHeaderListSize)

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)

	fr := buildHeadersFrame(t, 1, [][2]string{
		{":method", "GET"},
		{":path", "/"},
		{":scheme", "https"},
		{":authority", "example.com"},
	})

	err := sc.handleHeaderFrame(strm, fr)
	require.NoError(t, err)
	ReleaseFrameHeader(fr)
}

// ---- Trailer support -----------------------------------------------------

// TestHandleHeaderFrameTrailerAllowed verifies that a HEADERS frame with
// END_STREAM set is accepted as a trailer block after headers are finished.
func TestHandleHeaderFrameTrailerAllowed(t *testing.T) {
	sc := newTestServerConnWithWriter(4)
	sc.st.Reset()
	sc.st.SetMaxHeaderListSize(defaultMaxHeaderListSize)

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)
	// Simulate a fully processed request header block.
	strm.headersFinished = true
	strm.seenMethod = true
	strm.seenPath = true
	strm.seenScheme = true

	// Build a trailer frame: has END_STREAM + END_HEADERS, no pseudo-headers.
	fr := buildHeadersFrameWithOptions(t, 1, [][2]string{
		{"x-trailer", "value"},
	}, true /* endHeaders */, true /* endStream */)

	err := sc.handleHeaderFrame(strm, fr)
	require.NoError(t, err)
	ReleaseFrameHeader(fr)
}

// TestHandleHeaderFrameTrailerPseudoHeaderRejected verifies that a trailer
// containing a pseudo-header is rejected with ProtocolError.
func TestHandleHeaderFrameTrailerPseudoHeaderRejected(t *testing.T) {
	sc := newTestServerConnWithWriter(4)
	sc.st.Reset()
	sc.st.SetMaxHeaderListSize(defaultMaxHeaderListSize)

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)
	strm.headersFinished = true
	strm.seenMethod = true
	strm.seenPath = true
	strm.seenScheme = true

	// Trailers must not include pseudo-headers.
	fr := buildHeadersFrameWithOptions(t, 1, [][2]string{
		{":method", "GET"},
	}, true /* endHeaders */, true /* endStream */)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	ReleaseFrameHeader(fr)
}

// TestHandleHeaderFrameTrailerWithoutEndStream verifies that HEADERS after
// request headers without END_STREAM is rejected (not a valid trailer).
func TestHandleHeaderFrameTrailerWithoutEndStream(t *testing.T) {
	sc := newTestServerConnWithWriter(4)
	sc.st.Reset()

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)
	strm.headersFinished = true

	// END_HEADERS only (no END_STREAM) — not a valid trailer.
	fr := buildHeadersFrameWithOptions(t, 1, [][2]string{
		{"x-trailer", "value"},
	}, true /* endHeaders */, false /* endStream */)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)
	ReleaseFrameHeader(fr)
}

// ---- Raw header block size cap -------------------------------------------

// TestHandleHeaderFrameRawBlockSizeCap verifies that accumulating more than
// maxRawHeaderBlockSize raw bytes across fragments is rejected.
func TestHandleHeaderFrameRawBlockSizeCap(t *testing.T) {
	sc := newTestServerConnWithWriter(4)
	sc.st.Reset()

	strm := newTestStream(1)
	strm.SetState(StreamStateOpen)
	// Pre-fill previousHeaderBytes to just under the limit, then push over.
	strm.previousHeaderBytes = make([]byte, maxRawHeaderBlockSize)

	// Build a tiny frame whose addition would exceed the cap.
	fr := buildHeadersFrameWithOptions(t, 1, [][2]string{
		{":method", "GET"},
	}, false /* endHeaders */, false /* endStream */)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)
	ReleaseFrameHeader(fr)
}

// ---- ServerConfig defaults -----------------------------------------------

// TestServerConfigDefaults verifies that zero-value ServerConfig fields are
// set to their documented safe defaults.
func TestServerConfigDefaults(t *testing.T) {
	var cfg ServerConfig
	cfg.defaults()

	require.Equal(t, defaultMaxHeaderListSize, cfg.MaxHeaderListSize)
	require.Equal(t, defaultEnqueueTimeout, cfg.EnqueueTimeout)
	require.Equal(t, 10*time.Second, cfg.PingInterval)
	require.Equal(t, 1024, cfg.MaxConcurrentStreams)
}

// TestServerConfigCustomValues verifies that explicitly configured values are
// not overridden by defaults().
func TestServerConfigCustomValues(t *testing.T) {
	cfg := ServerConfig{
		MaxHeaderListSize:   64 * 1024,
		EnqueueTimeout:      5 * time.Second,
		MaxConcurrentStreams: 512,
		PingInterval:        30 * time.Second,
	}
	cfg.defaults()

	require.Equal(t, uint32(64*1024), cfg.MaxHeaderListSize)
	require.Equal(t, 5*time.Second, cfg.EnqueueTimeout)
	require.Equal(t, 512, cfg.MaxConcurrentStreams)
	require.Equal(t, 30*time.Second, cfg.PingInterval)
}
