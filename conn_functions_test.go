package http2

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestConnHandleSettingsAndPing(t *testing.T) {
	conn := &Conn{
		enc: AcquireHPACK(),
		out: make(chan *FrameHeader, 2),
	}
	defer ReleaseHPACK(conn.enc)

	st := &Settings{}
	st.SetHeaderTableSize(512)
	st.SetMaxConcurrentStreams(3)
	st.SetMaxWindowSize(128)

	conn.handleSettings(st)

	require.Equal(t, st.HeaderTableSize(), conn.serverS.HeaderTableSize())
	require.Equal(t, st.MaxConcurrentStreams(), conn.serverS.MaxConcurrentStreams())
	require.Equal(t, int32(st.MaxWindowSize()), conn.serverStreamWindow)

	fr := <-conn.out
	require.Equal(t, FrameSettings, fr.Type())
	require.True(t, fr.Body().(*Settings).IsAck())
	ReleaseFrameHeader(fr)

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()
	conn.handlePing(ping)

	fr = <-conn.out
	require.Equal(t, FramePing, fr.Type())
	require.True(t, fr.Body().(*Ping).IsAck())
	ReleaseFrameHeader(fr)
}

func TestConnHandleSettingsReplacesStreamWindow(t *testing.T) {
	conn := &Conn{
		enc: AcquireHPACK(),
		out: make(chan *FrameHeader, 2),
	}
	defer ReleaseHPACK(conn.enc)

	first := &Settings{}
	first.SetHeaderTableSize(512)
	first.SetMaxWindowSize(1024)

	conn.handleSettings(first)
	require.Equal(t, int32(first.MaxWindowSize()), conn.serverStreamWindow)

	second := &Settings{}
	second.SetHeaderTableSize(256)
	second.SetMaxWindowSize(2048)

	conn.handleSettings(second)
	require.Equal(t, int32(second.MaxWindowSize()), conn.serverStreamWindow)

	for i := 0; i < 2; i++ {
		fr := <-conn.out
		ReleaseFrameHeader(fr)
	}
}

func TestConnHandleSettingsRetainsStreamWindowWhenOmitted(t *testing.T) {
	conn := &Conn{
		enc: AcquireHPACK(),
		out: make(chan *FrameHeader, 2),
	}
	defer ReleaseHPACK(conn.enc)

	first := &Settings{}
	first.SetHeaderTableSize(512)
	first.SetMaxWindowSize(4096)
	conn.handleSettings(first)

	omitted := &Settings{}
	omitted.SetHeaderTableSize(256)
	conn.handleSettings(omitted)

	require.Equal(t, int32(first.MaxWindowSize()), conn.serverStreamWindow)
	require.Equal(t, first.MaxWindowSize(), conn.serverS.MaxWindowSize())

	for i := 0; i < 2; i++ {
		fr := <-conn.out
		ReleaseFrameHeader(fr)
	}
}

func TestConnHandleSettingsRetainsZeroStreamWindowWhenOmitted(t *testing.T) {
	conn := &Conn{
		enc: AcquireHPACK(),
		out: make(chan *FrameHeader, 2),
	}
	defer ReleaseHPACK(conn.enc)

	first := &Settings{}
	first.SetHeaderTableSize(512)
	first.SetMaxWindowSize(0)
	conn.handleSettings(first)

	omitted := &Settings{}
	omitted.SetHeaderTableSize(256)
	conn.handleSettings(omitted)

	require.Equal(t, int32(0), conn.serverStreamWindow)
	require.Equal(t, uint32(0), conn.serverS.MaxWindowSize())
	require.True(t, conn.serverS.HasMaxWindowSize())

	for i := 0; i < 2; i++ {
		fr := <-conn.out
		ReleaseFrameHeader(fr)
	}
}

func TestConnReadHeaderAndStream(t *testing.T) {
	conn := &Conn{
		dec:           AcquireHPACK(),
		out:           make(chan *FrameHeader, 3),
		maxWindow:     20,
		currentWindow: 20,
		serverWindow:  20,
	}
	defer ReleaseHPACK(conn.dec)
	conn.dec.DisableCompression = true

	enc := AcquireHPACK()
	enc.DisableCompression = true
	hf := AcquireHeaderField()
	headersFrame := &Headers{}
	hf.SetBytes(StringStatus, []byte("204"))
	enc.AppendHeaderField(headersFrame, hf, true)
	hf.SetBytes(StringContentLength, []byte("15"))
	enc.AppendHeaderField(headersFrame, hf, false)
	hf.SetBytes([]byte("x-test"), []byte("ok"))
	enc.AppendHeaderField(headersFrame, hf, false)
	headers := headersFrame.Headers()
	require.NotEmpty(t, headers)

	preview := AcquireHPACK()
	preview.DisableCompression = true
	tmp := headers
	hfPreview := AcquireHeaderField()
	found := map[string]string{}
	for len(tmp) > 0 {
		var err error
		tmp, err = preview.Next(hfPreview, tmp)
		require.NoError(t, err)
		found[hfPreview.Key()] = hfPreview.Value()
	}
	ReleaseHeaderField(hfPreview)
	ReleaseHPACK(preview)

	require.Equal(t, "204", found[":status"])
	require.Equal(t, "15", found["content-length"])
	require.Equal(t, "ok", found["x-test"])
	ReleaseHeaderField(hf)
	ReleaseHPACK(enc)

	conn.dec.Reset()
	conn.dec.DisableCompression = true

	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(res)

	require.NoError(t, conn.readHeader(headers, res))
	require.Equal(t, 204, res.StatusCode())
	require.Equal(t, "ok", string(res.Header.Peek("x-test")))

	fr := AcquireFrameHeader()
	data := &Data{}
	data.SetData([]byte("1234567890abcde"))
	data.SetEndStream(true)
	fr.SetBody(data)
	fr.SetStream(1)

	data.Serialize(fr)
	fr.length = len(fr.payload)

	require.NoError(t, conn.readStream(fr, res))
	require.Equal(t, "1234567890abcde", string(res.Body()))

	first := <-conn.out
	require.Equal(t, FrameWindowUpdate, first.Type())
	ReleaseFrameHeader(first)

	second := <-conn.out
	require.Equal(t, FrameWindowUpdate, second.Type())
	ReleaseFrameHeader(second)
}

func TestConnReadStreamSendsWindowUpdateForPaddingOnlyData(t *testing.T) {
	conn := &Conn{
		out:           make(chan *FrameHeader, 2),
		maxWindow:     10,
		currentWindow: 10,
		serverWindow:  10,
	}

	res := fasthttp.AcquireResponse()
	t.Cleanup(func() {
		fasthttp.ReleaseResponse(res)
	})

	fr := AcquireFrameHeader()
	data := &Data{}
	fr.SetBody(data)
	fr.SetStream(1)
	fr.SetFlags(FlagPadded)
	fr.length = 4 // one byte padding length + three padding bytes
	t.Cleanup(func() {
		ReleaseFrameHeader(fr)
	})

	require.NoError(t, conn.readStream(fr, res))
	require.Empty(t, res.Body())
	require.Equal(t, int32(6), conn.currentWindow)
	require.Equal(t, int32(6), conn.serverWindow)

	streamWU := <-conn.out
	require.Equal(t, FrameWindowUpdate, streamWU.Type())
	require.Equal(t, uint32(1), streamWU.Stream())
	require.Equal(t, 4, streamWU.Body().(*WindowUpdate).Increment())
	ReleaseFrameHeader(streamWU)

	require.Zero(t, len(conn.out))
}

func TestConnReadStreamUpdatesConnectionWindowWithPaddingOnlyData(t *testing.T) {
	conn := &Conn{
		out:           make(chan *FrameHeader, 2),
		maxWindow:     10,
		currentWindow: 10,
		serverWindow:  10,
	}

	res := fasthttp.AcquireResponse()
	t.Cleanup(func() {
		fasthttp.ReleaseResponse(res)
	})

	fr := AcquireFrameHeader()
	data := &Data{}
	fr.SetBody(data)
	fr.SetStream(3)
	fr.SetFlags(FlagPadded)
	fr.length = 7 // one byte padding length + six padding bytes
	t.Cleanup(func() {
		ReleaseFrameHeader(fr)
	})

	require.NoError(t, conn.readStream(fr, res))
	require.Empty(t, res.Body())
	require.Equal(t, int32(10), conn.currentWindow)
	require.Equal(t, int32(3), conn.serverWindow)

	streamWU := <-conn.out
	require.Equal(t, FrameWindowUpdate, streamWU.Type())
	require.Equal(t, uint32(3), streamWU.Stream())
	require.Equal(t, 7, streamWU.Body().(*WindowUpdate).Increment())
	ReleaseFrameHeader(streamWU)

	connWU := <-conn.out
	require.Equal(t, FrameWindowUpdate, connWU.Type())
	require.Equal(t, uint32(0), connWU.Stream())
	require.Equal(t, 7, connWU.Body().(*WindowUpdate).Increment())
	ReleaseFrameHeader(connWU)

	require.Zero(t, len(conn.out))
}

func TestConnWriteDataRespectsFlowControl(t *testing.T) {
	raw := &bytes.Buffer{}
	conn := &Conn{
		bw:           bufio.NewWriter(raw),
		serverWindow: 4,
	}

	ctx := &Ctx{
		Err: make(chan error, 1),
	}
	atomic.StoreInt32(&ctx.sendWindow, 3)

	fh := AcquireFrameHeader()
	defer ReleaseFrameHeader(fh)
	fh.SetStream(1)

	body := []byte("abcdefghij")
	done := make(chan error, 1)
	go func() {
		done <- conn.writeData(fh, ctx, body)
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		require.NoError(t, err)
		require.Fail(t, "writeData completed before window updates applied")
	default:
	}

	atomic.AddInt32(&conn.serverWindow, 5)
	atomic.AddInt32(&ctx.sendWindow, 5)
	conn.notifyWindowAvailable()

	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		require.NoError(t, err)
		require.Fail(t, "writeData completed before final window update")
	default:
	}

	atomic.AddInt32(&conn.serverWindow, 5)
	atomic.AddInt32(&ctx.sendWindow, 5)
	conn.notifyWindowAvailable()

	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			require.NoError(t, err)
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, conn.bw.Flush())

	reader := bufio.NewReader(bytes.NewReader(raw.Bytes()))

	var (
		lengths  []int
		payload  []byte
		endFlags []bool
	)

	for {
		fr, err := ReadFrameFrom(reader)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.Equal(t, FrameData, fr.Type())

		data := fr.Body().(*Data)
		lengths = append(lengths, fr.Len())
		endFlags = append(endFlags, fr.Flags().Has(FlagEndStream))
		payload = append(payload, data.Data()...)

		ReleaseFrameHeader(fr)
	}

	require.Equal(t, []int{3, 5, 2}, lengths)
	require.Equal(t, []bool{false, false, true}, endFlags)
	require.Equal(t, body, payload)
}

func TestConnWriteDataTimesOutWhenWindowStalled(t *testing.T) {
	conn := &Conn{
		bw: bufio.NewWriter(io.Discard),
	}
	atomic.StoreInt32(&conn.serverWindow, 0)

	ctx := &Ctx{
		Err: make(chan error, 1),
	}
	atomic.StoreInt32(&ctx.sendWindow, 0)

	fh := AcquireFrameHeader()
	defer ReleaseFrameHeader(fh)
	fh.SetStream(1)

	start := time.Now()
	err := conn.writeData(fh, ctx, []byte("abc"))
	elapsed := time.Since(start)

	require.ErrorIs(t, err, FlowControlError)
	require.GreaterOrEqual(t, elapsed, windowWaitTimeout)
}

func TestConnWriteDataUnblocksOnClose(t *testing.T) {
	conn := &Conn{
		bw: bufio.NewWriter(io.Discard),
		c:  &stubConn{},
		in: make(chan *Ctx),
	}
	atomic.StoreInt32(&conn.serverWindow, 0)

	ctx := &Ctx{
		Err: make(chan error, 1),
	}
	atomic.StoreInt32(&ctx.sendWindow, 0)

	fh := AcquireFrameHeader()
	defer ReleaseFrameHeader(fh)
	fh.SetStream(1)

	done := make(chan error, 1)
	go func() {
		done <- conn.writeData(fh, ctx, []byte("abc"))
	}()

	time.Sleep(50 * time.Millisecond)
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool {
		select {
		case werr := <-done:
			require.ErrorIs(t, werr, net.ErrClosed)
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
}

func TestConnWritePing(t *testing.T) {
	var buf bytes.Buffer
	conn := &Conn{
		bw: bufio.NewWriter(&buf),
	}

	require.NoError(t, conn.writePing())
	require.Greater(t, buf.Len(), 0)
	require.Equal(t, int32(1), atomic.LoadInt32(&conn.unacks))
}

func TestConnFinishResolves(t *testing.T) {
	conn := &Conn{
		openStreams: 2,
	}
	ctx := &Ctx{Err: make(chan error, 1)}

	conn.finish(ctx, 1, io.EOF)

	require.Equal(t, int32(1), atomic.LoadInt32(&conn.openStreams))
	require.ErrorIs(t, <-ctx.Err, io.EOF)
}

func TestClientOnConnectionDroppedRemovesConn(t *testing.T) {
	dialErr := errors.New("dial failure")
	client := createClient(&Dialer{
		Addr: "example.com:443",
		NetDial: func(string) (net.Conn, error) {
			return nil, dialErr
		},
	}, ClientOpts{})

	conn := &Conn{}
	client.conns.PushBack(conn)

	client.onConnectionDropped(conn)

	require.Zero(t, client.conns.Len())
}

func TestConnSetOnDisconnectAndLastErr(t *testing.T) {
	conn := &Conn{
		c:  &stubConn{},
		bw: bufio.NewWriter(io.Discard),
		in: make(chan *Ctx),
	}
	conn.setLastErr(io.ErrUnexpectedEOF)
	require.ErrorIs(t, conn.LastErr(), io.ErrUnexpectedEOF)

	closed := make(chan struct{})
	conn.SetOnDisconnect(func(*Conn) { close(closed) })

	require.NoError(t, conn.Close())
	<-closed
}

func TestWriteErrorWrapping(t *testing.T) {
	we := WriteError{err: customErr{}}
	require.EqualError(t, we, "writing error: custom")
	require.ErrorIs(t, we, customErr{})

	var target customErr
	require.True(t, errors.As(we, &target))
}

func TestConnLastErrConcurrentAccess(t *testing.T) {
	conn := &Conn{}
	lastErr := errors.New("writer error")

	var wg sync.WaitGroup
	writers := 8
	readers := 8
	iterations := 1_000

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				conn.setLastErr(lastErr)
			}
		}()
	}

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = conn.LastErr()
			}
		}()
	}

	wg.Wait()

	require.ErrorIs(t, conn.LastErr(), lastErr)
}

func TestWriteLoopPreservesExistingLastErr(t *testing.T) {
	conn := &Conn{
		in: make(chan *Ctx),
		bw: bufio.NewWriter(io.Discard),
	}

	readErr := errors.New("read failure")
	conn.setLastErr(readErr)
	atomic.StoreUint64(&conn.closed, 1)

	done := make(chan struct{})
	go func() {
		conn.writeLoop()
		close(done)
	}()

	close(conn.in)

	<-done

	require.ErrorIs(t, conn.LastErr(), readErr)
}

type errWriteConn struct {
	stubConn
}

func (c *errWriteConn) Write([]byte) (int, error) { return 0, net.ErrClosed }

func TestWriteLoopPrefersStoredErrorOverClosedWrite(t *testing.T) {
	raw := &errWriteConn{}
	conn := &Conn{
		c:            raw,
		bw:           bufio.NewWriter(raw),
		in:           make(chan *Ctx),
		out:          make(chan *FrameHeader, 1),
		pingInterval: time.Hour,
	}

	readErr := errors.New("read loop failure")
	conn.setLastErr(readErr)

	done := make(chan struct{})
	go func() {
		conn.writeLoop()
		close(done)
	}()

	fr := AcquireFrameHeader()
	fr.SetBody(AcquireFrame(FrameSettings))
	conn.out <- fr

	<-done

	require.ErrorIs(t, conn.LastErr(), readErr)
}

func TestConnSettingsUpdateLimitsStreamsDuringRequests(t *testing.T) {
	var buf bytes.Buffer
	conn := &Conn{
		bw:     bufio.NewWriter(&buf),
		enc:    AcquireHPACK(),
		out:    make(chan *FrameHeader, 1),
		nextID: 1,
	}
	defer ReleaseHPACK(conn.enc)

	conn.serverS.Reset()
	conn.serverS.SetMaxConcurrentStreams(2)
	atomic.StoreInt32(&conn.openStreams, 1)

	st := &Settings{}
	st.Reset()
	st.SetMaxConcurrentStreams(1)

	start := make(chan struct{})
	settingsDone := make(chan struct{})
	go func() {
		<-start
		conn.handleSettings(st)
		close(settingsDone)
	}()

	checksDone := make(chan struct{})
	go func() {
		<-start
		// Small delay to ensure settings are being processed
		time.Sleep(10 * time.Millisecond)
		for {
			if !conn.CanOpenStream() {
				close(checksDone)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	close(start)
	select {
	case <-settingsDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for settings handling to finish")
	}

	select {
	case <-checksDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for stream limit enforcement")
	}

	select {
	case fr := <-conn.out:
		require.Equal(t, FrameSettings, fr.Type())
		require.True(t, fr.Body().(*Settings).IsAck())
		ReleaseFrameHeader(fr)
	default:
		t.Fatal("expected settings ack frame")
	}

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://example.com/test")
	req.Header.SetMethod("GET")

	ctx := &Ctx{
		Request:  req,
		Response: res,
		Err:      make(chan error, 1),
	}

	require.ErrorIs(t, conn.writeRequest(ctx), ErrNotAvailableStreams)
	require.Equal(t, int32(1), atomic.LoadInt32(&conn.openStreams))
}

func TestConnWriteRequest(t *testing.T) {
	rawConn := &stubConn{}
	conn := NewConn(rawConn, ConnOpts{})
	conn.serverS.SetMaxConcurrentStreams(2)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://example.com/test")
	req.Header.SetMethod("POST")
	req.SetBody([]byte("hello world"))
	req.Header.Set("X-Test", "value")
	req.Header.SetUserAgent("ua")

	ctx := &Ctx{
		Request:  req,
		Response: res,
		Err:      make(chan error, 1),
	}

	require.NoError(t, conn.writeRequest(ctx))
	require.Greater(t, rawConn.Buffer.Len(), 0)
	require.Positive(t, atomic.LoadUint32(&ctx.streamID))

	_, ok := conn.reqQueued.Load(ctx.streamID)
	require.True(t, ok)
}

type stubConn struct {
	bytes.Buffer
}

func (c *stubConn) Read(p []byte) (int, error)  { return 0, io.EOF }
func (c *stubConn) Write(p []byte) (int, error) { return c.Buffer.Write(p) }
func (c *stubConn) Close() error                { return nil }
func (c *stubConn) LocalAddr() net.Addr         { return &net.TCPAddr{} }
func (c *stubConn) RemoteAddr() net.Addr        { return &net.TCPAddr{} }
func (c *stubConn) SetDeadline(time.Time) error { return nil }
func (c *stubConn) SetReadDeadline(time.Time) error {
	return nil
}
func (c *stubConn) SetWriteDeadline(time.Time) error {
	return nil
}

type customErr struct{}

func (customErr) Error() string { return "custom" }
