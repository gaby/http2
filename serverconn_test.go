package http2

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func newTestServerConn() *serverConn {
	sc := &serverConn{
		logger: log.New(io.Discard, "", 0),
	}
	sc.dec.Reset()
	return sc
}

func newTestStream(id uint32) *Stream {
	strm := NewStream(id, 0, 0)
	strm.ctx = &fasthttp.RequestCtx{}
	strm.ctx.Request.Reset()
	return strm
}

func buildHeadersFrame(t *testing.T, streamID uint32, headers [][2]string) *FrameHeader {
	t.Helper()

	var hp HPACK
	hp.Reset()
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	h := AcquireFrame(FrameHeaders).(*Headers)
	for _, kv := range headers {
		hf.Set(kv[0], kv[1])
		h.AppendHeaderField(&hp, hf, true)
	}
	h.SetEndHeaders(true)

	fr := AcquireFrameHeader()
	fr.SetStream(streamID)
	var flags FrameFlags
	if h.EndStream() {
		flags = flags.Add(FlagEndStream)
	}
	if h.EndHeaders() {
		flags = flags.Add(FlagEndHeaders)
	}
	if h.Priority() {
		flags = flags.Add(FlagPriority)
	}
	if h.Padding() {
		flags = flags.Add(FlagPadded)
	}
	fr.SetFlags(flags)
	fr.SetBody(h)
	fr.length = len(h.Headers())
	return fr
}

func TestServerConnPingAndErrors(t *testing.T) {
	writer := make(chan *FrameHeader, 4)
	sc := &serverConn{
		writer: writer,
	}

	ping := &Ping{}
	sc.handlePing(ping)

	fr := <-writer
	require.Equal(t, FramePing, fr.Type())
	require.True(t, fr.Body().(*Ping).IsAck())
	ReleaseFrameHeader(fr)

	sc.writePing()
	fr = <-writer
	require.Equal(t, FramePing, fr.Type())
	ReleaseFrameHeader(fr)

	strm := NewStream(1, 10, 10)
	sc.writeError(strm, errors.New("boom"))
	fr = <-writer
	require.Equal(t, FrameResetStream, fr.Type())
	require.Equal(t, StreamStateClosed, strm.State())
	ReleaseFrameHeader(fr)

	sc.writeError(nil, NewGoAwayError(ProtocolError, "fail"))
	fr = <-writer
	require.Equal(t, FrameGoAway, fr.Type())
	ReleaseFrameHeader(fr)
}

func TestHandleFrameDataStreamFlowControl(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		logger: log.New(io.Discard, "", 0),
	}
	sc.maxWindow = 10
	sc.currentWindow = sc.maxWindow

	strm := NewStream(1, 3, 3)
	strm.SetState(StreamStateOpen)
	strm.headersFinished = true
	strm.ctx = &fasthttp.RequestCtx{}
	strm.ctx.Request.Reset()

	data := AcquireFrame(FrameData).(*Data)
	data.SetData([]byte("hello"))

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	fr.SetBody(data)
	fr.length = len(data.Data())

	require.NoError(t, sc.consumeConnectionWindow(data.Len()))
	err := sc.handleFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, FlowControlError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)

	sc.writeError(strm, err)
	out := <-sc.writer
	require.Equal(t, FrameResetStream, out.Type())
	require.Equal(t, FlowControlError, out.Body().(*RstStream).Code())
	ReleaseFrameHeader(out)
	ReleaseFrameHeader(fr)
}

func TestConnectionWindowUnderflowSendsGoAway(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		logger: log.New(io.Discard, "", 0),
	}
	sc.maxWindow = 4
	sc.currentWindow = sc.maxWindow

	err := sc.consumeConnectionWindow(8)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, FlowControlError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)

	sc.writeError(nil, err)
	fr := <-sc.writer
	require.Equal(t, FrameGoAway, fr.Type())
	require.Equal(t, FlowControlError, fr.Body().(*GoAway).Code())
	ReleaseFrameHeader(fr)
}

func TestHandleFrameSendsWindowUpdatesAfterReading(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 4),
		logger: log.New(io.Discard, "", 0),
	}
	sc.maxWindow = 20
	sc.currentWindow = sc.maxWindow

	strm := NewStream(1, 10, 10)
	strm.SetState(StreamStateOpen)
	strm.headersFinished = true
	strm.ctx = &fasthttp.RequestCtx{}
	strm.ctx.Request.Reset()

	payload := []byte("data")
	data := AcquireFrame(FrameData).(*Data)
	data.SetData(payload)

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	fr.SetBody(data)
	fr.length = len(payload)

	require.NoError(t, sc.consumeConnectionWindow(len(payload)))
	err := sc.handleFrame(strm, fr)
	require.NoError(t, err)

	connWU := <-sc.writer
	streamWU := <-sc.writer

	require.Equal(t, FrameWindowUpdate, connWU.Type())
	require.EqualValues(t, 0, connWU.Stream())
	require.Equal(t, len(payload), connWU.Body().(*WindowUpdate).Increment())

	require.Equal(t, FrameWindowUpdate, streamWU.Type())
	require.Equal(t, strm.ID(), streamWU.Stream())
	require.Equal(t, len(payload), streamWU.Body().(*WindowUpdate).Increment())

	require.Equal(t, int32(20), sc.currentWindow)
	require.Equal(t, int32(10), strm.Window())

	ReleaseFrameHeader(connWU)
	ReleaseFrameHeader(streamWU)
	ReleaseFrameHeader(fr)
}

func TestPaddedDataCountsAgainstWindow(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		logger: log.New(io.Discard, "", 0),
	}
	sc.maxWindow = 15
	sc.currentWindow = sc.maxWindow

	strm := NewStream(1, 10, 10)
	strm.SetState(StreamStateOpen)
	strm.headersFinished = true
	strm.ctx = &fasthttp.RequestCtx{}
	strm.ctx.Request.Reset()

	data := AcquireFrame(FrameData).(*Data)
	data.SetPadding(true)
	data.SetData([]byte("data"))

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	fr.SetBody(data)
	fr.length = len(data.Data()) + 1 + 10 // pad length field + padding bytes

	require.NoError(t, sc.consumeConnectionWindow(fr.Len()))
	err := sc.handleFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, FlowControlError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)

	sc.writeError(strm, err)
	out := <-sc.writer
	require.Equal(t, FrameResetStream, out.Type())
	ReleaseFrameHeader(out)
	ReleaseFrameHeader(fr)
}

func TestStreamWriteHelpers(t *testing.T) {
	writer := make(chan *FrameHeader, 4)
	strm := NewStream(3, 100, 100)

	sw := acquireStreamWrite()
	sw.size = int64(len("hello"))
	sw.strm = strm
	sw.writer = writer

	n, err := sw.Write([]byte("hello"))
	require.NoError(t, err)
	require.Equal(t, 5, n)
	fr := <-writer
	require.Equal(t, FrameData, fr.Type())
	ReleaseFrameHeader(fr)
	releaseStreamWrite(sw)

	lr := &io.LimitedReader{
		R: bytes.NewBufferString("abc"),
		N: 3,
	}

	sw = acquireStreamWrite()
	sw.size = -1
	sw.strm = strm
	sw.writer = writer

	num, err := sw.ReadFrom(lr)
	require.NoError(t, err)
	require.EqualValues(t, 3, num)

	fr = <-writer
	require.Equal(t, FrameData, fr.Type())
	ReleaseFrameHeader(fr)
	releaseStreamWrite(sw)
}

func TestStreamsAndWindowUpdateHelpers(t *testing.T) {
	s1 := NewStream(1, 0, 0)
	s1.origType = FrameHeaders
	s2 := NewStream(2, 0, 0)
	s2.origType = FrameData

	strms := Streams{s1, s2}
	require.Equal(t, s1, strms.GetFirstOf(FrameHeaders))
	require.Nil(t, strms.GetFirstOf(FramePriority))

	wu := &WindowUpdate{}
	wu.SetIncrement(10)
	var copied WindowUpdate
	wu.CopyTo(&copied)
	require.Equal(t, wu.Increment(), copied.Increment())
}

func TestHandleStreamsConcurrentSettings(t *testing.T) {
	sc := &serverConn{
		reader:          make(chan *FrameHeader, 64),
		writer:          make(chan *FrameHeader, 64),
		maxRequestTimer: time.NewTimer(time.Hour),
		maxRequestTime:  time.Hour,
		closer:          make(chan struct{}),
	}

	sc.st.Reset()
	sc.clientS.Reset()
	atomic.StoreInt64(&sc.clientWindow, int64(sc.clientS.MaxWindowSize()))

	writerDone := make(chan struct{})
	go func() {
		for fr := range sc.writer {
			ReleaseFrameHeader(fr)
		}
		close(writerDone)
	}()

	streamsDone := make(chan struct{})
	go func() {
		sc.handleStreams()
		close(streamsDone)
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			st := &Settings{}
			st.Reset()
			st.SetMaxWindowSize(defaultWindowSize + uint32(i))
			sc.handleSettings(st)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			fr := AcquireFrameHeader()
			fr.SetStream(uint32(i*2 + 1))

			priority := AcquireFrame(FramePriority).(*Priority)
			priority.SetStream(0)
			fr.SetBody(priority)

			sc.reader <- fr
		}
	}()

	wg.Wait()
	close(sc.closer)
	<-streamsDone

	close(sc.writer)
	<-writerDone
}

func TestServerConnSettingsWhileEncoding(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 256),
		logger: log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.clientS.Reset()
	sc.enc.Reset()

	var respCounter uint32
	sc.h = func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Reset()
		current := atomic.AddUint32(&respCounter, 1)
		ctx.Response.Header.Set("X-Test", strconv.Itoa(int(current)))
		ctx.Response.SetStatusCode(200)
		ctx.Response.SetBodyString("hello")
	}

	srvConn, cliConn := net.Pipe()
	defer srvConn.Close()
	defer cliConn.Close()

	strm := NewStream(1, int32(defaultWindowSize), int32(defaultWindowSize))
	sc.createStream(srvConn, FrameHeaders, strm)

	writerDone := make(chan struct{})
	go func() {
		for fr := range sc.writer {
			ReleaseFrameHeader(fr)
		}
		close(writerDone)
	}()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			st := &Settings{}
			st.Reset()
			st.SetHeaderTableSize(defaultHeaderTableSize + uint32(i))
			sc.handleSettings(st)
		}
	}()

	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			sc.handleEndRequest(strm)
		}
	}()

	close(start)
	wg.Wait()
	close(sc.writer)
	<-writerDone
}

func TestServerConnFrameTooLargeSendsReset(t *testing.T) {
	const maxSize = 16
	const oversized = maxSize + 1

	header := []byte{
		0x0, 0x0, byte(oversized),
		byte(FrameData), 0x0,
		0x0, 0x0, 0x0, 0x1,
	}
	payload := bytes.Repeat([]byte{0}, oversized)
	data := append(header, payload...)

	sc := &serverConn{
		br:     bufio.NewReader(bytes.NewReader(data)),
		st:     Settings{},
		writer: make(chan *FrameHeader, 1),
		logger: log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.st.SetMaxFrameSize(maxSize)

	err := sc.readLoop()
	require.ErrorIs(t, err, ErrPayloadExceeds)

	fr := <-sc.writer
	require.Equal(t, FrameResetStream, fr.Type())
	require.Equal(t, FrameSizeError, fr.Body().(*RstStream).Code())
	ReleaseFrameHeader(fr)
}

func TestServerConnFrameSizeUsesServerSettings(t *testing.T) {
	const maxSize = 32

	header := []byte{
		0x0, 0x0, byte(maxSize),
		byte(FrameData), 0x1,
		0x0, 0x0, 0x0, 0x1,
	}
	payload := bytes.Repeat([]byte{0}, maxSize)
	data := append(header, payload...)

	sc := &serverConn{
		br:      bufio.NewReader(bytes.NewReader(data)),
		st:      Settings{},
		clientS: Settings{},
		writer:  make(chan *FrameHeader, 1),
		reader:  make(chan *FrameHeader, 1),
		logger:  log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.st.SetMaxFrameSize(maxSize)
	sc.clientS.Reset()
	sc.clientS.SetMaxFrameSize(maxSize / 2)

	err := sc.readLoop()
	require.True(t, err == nil || errors.Is(err, io.EOF))

	fr := <-sc.reader
	require.Equal(t, FrameData, fr.Type())
	ReleaseFrameHeader(fr)
}

func TestHandleHeaderFramePseudoAfterRegular(t *testing.T) {
	sc := newTestServerConn()
	strm := newTestStream(1)

	fr := buildHeadersFrame(t, strm.ID(), [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{"foo", "bar"},
		{":path", "/"},
	})
	defer ReleaseFrameHeader(fr)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)
}

func TestHandleHeaderFrameDuplicatePseudo(t *testing.T) {
	sc := newTestServerConn()
	strm := newTestStream(1)

	fr := buildHeadersFrame(t, strm.ID(), [][2]string{
		{":method", "GET"},
		{":method", "POST"},
		{":scheme", "https"},
		{":path", "/"},
	})
	defer ReleaseFrameHeader(fr)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)
}

func TestHandleHeaderFrameForbiddenHeaders(t *testing.T) {
	tests := []struct {
		name   string
		header [2]string
	}{
		{name: "connection", header: [2]string{"connection", "close"}},
		{name: "upgrade", header: [2]string{"upgrade", "h2c"}},
		{name: "proxy-connection", header: [2]string{"proxy-connection", "keep-alive"}},
		{name: "transfer-encoding", header: [2]string{"transfer-encoding", "chunked"}},
		{name: "te", header: [2]string{"te", "gzip"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newTestServerConn()
			strm := newTestStream(1)

			fr := buildHeadersFrame(t, strm.ID(), [][2]string{
				{":method", "GET"},
				{":scheme", "https"},
				{":path", "/"},
				tt.header,
			})
			defer ReleaseFrameHeader(fr)

			err := sc.handleHeaderFrame(strm, fr)
			require.Error(t, err)
			var h2Err Error
			require.ErrorAs(t, err, &h2Err)
			require.Equal(t, ProtocolError, h2Err.Code())
			require.Equal(t, FrameGoAway, h2Err.frameType)
		})
	}
}

func TestHandleHeaderFrameMissingRequiredPseudo(t *testing.T) {
	sc := newTestServerConn()
	strm := newTestStream(1)

	fr := buildHeadersFrame(t, strm.ID(), [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
	})
	defer ReleaseFrameHeader(fr)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)
}

func TestHandleHeaderFrameUnknownPseudo(t *testing.T) {
	sc := newTestServerConn()
	strm := newTestStream(1)

	fr := buildHeadersFrame(t, strm.ID(), [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":path", "/"},
		{":status", "200"},
	})
	defer ReleaseFrameHeader(fr)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)
}

func TestHandleHeaderFrameInvalidCompressionIsConnectionError(t *testing.T) {
	sc := newTestServerConn()
	strm := newTestStream(1)

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetHeaders([]byte{0x40})
	h.SetEndHeaders(true)

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	fr.SetFlags(FrameFlags(0).Add(FlagEndHeaders))
	fr.SetBody(h)
	fr.length = len(h.Headers())

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, CompressionError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)

	ReleaseFrameHeader(fr)
}

func TestHandleHeaderFrameConnectAllowsSchemeAndPathOmission(t *testing.T) {
	sc := newTestServerConn()
	strm := newTestStream(1)

	fr := buildHeadersFrame(t, strm.ID(), [][2]string{
		{":method", "CONNECT"},
		{":authority", "example.com:443"},
	})
	defer ReleaseFrameHeader(fr)

	err := sc.handleHeaderFrame(strm, fr)
	require.NoError(t, err)
}

func TestHandleHeaderFrameConnectRequiresAuthority(t *testing.T) {
	sc := newTestServerConn()
	strm := newTestStream(1)

	fr := buildHeadersFrame(t, strm.ID(), [][2]string{
		{":method", "CONNECT"},
	})
	defer ReleaseFrameHeader(fr)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)
}

func TestHandleHeaderFrameConnectRejectsPathAndScheme(t *testing.T) {
	sc := newTestServerConn()
	strm := newTestStream(1)

	fr := buildHeadersFrame(t, strm.ID(), [][2]string{
		{":method", "CONNECT"},
		{":authority", "example.com:443"},
		{":path", "/"},
	})
	defer ReleaseFrameHeader(fr)

	err := sc.handleHeaderFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)
}
