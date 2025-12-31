package http2

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
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
	conn.lastErr = io.ErrUnexpectedEOF
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
		time.Sleep(time.Millisecond * 10)
		conn.handleSettings(st)
		close(settingsDone)
	}()

	checksDone := make(chan struct{})
	go func() {
		<-start
		for i := 0; i < 100; i++ {
			conn.CanOpenStream()
			time.Sleep(time.Millisecond)
		}
		close(checksDone)
	}()

	close(start)
	<-settingsDone
	<-checksDone

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
