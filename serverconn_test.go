package http2

import (
	"bufio"
	"bytes"
	"encoding/binary"
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

type writeFailConn struct {
	net.Conn
	writeErr  error
	closed    atomic.Bool
	closeOnce sync.Once
}

func (c *writeFailConn) Write(_ []byte) (int, error) {
	return 0, c.writeErr
}

func (c *writeFailConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.Conn != nil {
			err = c.Conn.Close()
		}
	})
	return err
}

func buildHeadersFrame(t *testing.T, streamID uint32, headers [][2]string) *FrameHeader {
	return buildHeadersFrameWithOptions(t, streamID, headers, true, false)
}

func buildHeadersFrameWithOptions(t *testing.T, streamID uint32, headers [][2]string, endHeaders, endStream bool) *FrameHeader {
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
	h.SetEndHeaders(endHeaders)
	h.SetEndStream(endStream)

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

func buildWindowUpdateFrame(_ *testing.T, streamID uint32, increment uint32) []byte {
	header := []byte{
		0x0, 0x0, 0x4,
		byte(FrameWindowUpdate), 0x0,
		byte(streamID >> 24), byte(streamID >> 16), byte(streamID >> 8), byte(streamID),
	}
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, increment)

	return append(header, payload...)
}

func appendEmptySettingsFrame(payload []byte) []byte {
	settings := []byte{
		0x0, 0x0, 0x0,
		byte(FrameSettings), 0x0,
		0x0, 0x0, 0x0, 0x0,
	}
	return append(settings, payload...)
}

func writeEmptySettingsFrame(t *testing.T, w *bufio.Writer) {
	t.Helper()
	_, err := w.Write([]byte{
		0x0, 0x0, 0x0,
		byte(FrameSettings), 0x0,
		0x0, 0x0, 0x0, 0x0,
	})
	require.NoError(t, err)
}

func TestFirstFrameMustBeSettings(t *testing.T) {
	fr := buildHeadersFrame(t, 1, [][2]string{{"a", "b"}})
	var b bytes.Buffer
	bw := bufio.NewWriter(&b)
	_, err := fr.WriteTo(bw)
	require.NoError(t, err)
	require.NoError(t, bw.Flush())
	ReleaseFrameHeader(fr)

	sc := &serverConn{
		br:      bufio.NewReader(bytes.NewReader(b.Bytes())),
		st:      Settings{},
		clientS: Settings{},
		writer:  make(chan *FrameHeader, 1),
		logger:  log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.clientS.Reset()

	err = sc.readLoop()
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)

	fr = <-sc.writer
	require.Equal(t, FrameGoAway, fr.Type())
	require.Equal(t, ProtocolError, fr.Body().(*GoAway).Code())
	ReleaseFrameHeader(fr)
}

func TestFirstFrameSettingsAckSendsGoAway(t *testing.T) {
	var st Settings
	st.SetAck(true)

	var b bytes.Buffer
	bw := bufio.NewWriter(&b)

	fr := AcquireFrameHeader()
	fr.SetBody(&st)
	_, err := fr.WriteTo(bw)
	require.NoError(t, err)
	require.NoError(t, bw.Flush())
	ReleaseFrameHeader(fr)

	sc := &serverConn{
		br:      bufio.NewReader(bytes.NewReader(b.Bytes())),
		st:      Settings{},
		clientS: Settings{},
		writer:  make(chan *FrameHeader, 1),
		logger:  log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.clientS.Reset()

	err = sc.readLoop()
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)

	fr = <-sc.writer
	require.Equal(t, FrameGoAway, fr.Type())
	require.Equal(t, ProtocolError, fr.Body().(*GoAway).Code())
	ReleaseFrameHeader(fr)
}

func TestFirstFrameSettingsContinues(t *testing.T) {
	var b bytes.Buffer
	bw := bufio.NewWriter(&b)

	settings := Settings{}
	fr := AcquireFrameHeader()
	fr.SetBody(&settings)
	_, err := fr.WriteTo(bw)
	require.NoError(t, err)
	ReleaseFrameHeader(fr)

	ping := &Ping{}
	fr = AcquireFrameHeader()
	fr.SetBody(ping)
	_, err = fr.WriteTo(bw)
	require.NoError(t, err)
	require.NoError(t, bw.Flush())
	ReleaseFrameHeader(fr)

	sc := &serverConn{
		br:      bufio.NewReader(bytes.NewReader(b.Bytes())),
		st:      Settings{},
		clientS: Settings{},
		writer:  make(chan *FrameHeader, 2),
		logger:  log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.clientS.Reset()

	err = sc.readLoop()
	require.ErrorIs(t, err, io.EOF)

	fr = <-sc.writer
	require.Equal(t, FrameSettings, fr.Type())
	require.True(t, fr.Body().(*Settings).IsAck())
	ReleaseFrameHeader(fr)

	fr = <-sc.writer
	require.Equal(t, FramePing, fr.Type())
	require.True(t, fr.Body().(*Ping).IsAck())
	ReleaseFrameHeader(fr)
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

func TestConnectionWindowUpdateOverflowSendsFlowControlGoAway(t *testing.T) {
	frame := appendEmptySettingsFrame(buildWindowUpdateFrame(t, 0, uint32(maxWindowIncrement)))
	sc := &serverConn{
		br:      bufio.NewReader(bytes.NewReader(frame)),
		st:      Settings{},
		clientS: Settings{},
		writer:  make(chan *FrameHeader, 2),
		logger:  log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.clientS.Reset()
	atomic.StoreInt64(&sc.clientWindow, int64(sc.clientS.MaxWindowSize()))

	err := sc.readLoop()
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, FlowControlError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)
	require.EqualValues(t, maxWindowIncrement, atomic.LoadInt64(&sc.clientWindow))

	fr := <-sc.writer
	require.Equal(t, FrameSettings, fr.Type())
	require.True(t, fr.Body().(*Settings).IsAck())
	ReleaseFrameHeader(fr)

	fr = <-sc.writer
	require.Equal(t, FrameGoAway, fr.Type())
	require.Equal(t, FlowControlError, fr.Body().(*GoAway).Code())
	ReleaseFrameHeader(fr)
}

func TestConnectionWindowUpdateZeroSendsProtocolGoAway(t *testing.T) {
	frame := appendEmptySettingsFrame(buildWindowUpdateFrame(t, 0, 0))
	sc := &serverConn{
		br:      bufio.NewReader(bytes.NewReader(frame)),
		st:      Settings{},
		clientS: Settings{},
		writer:  make(chan *FrameHeader, 2),
		logger:  log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.clientS.Reset()
	atomic.StoreInt64(&sc.clientWindow, int64(sc.clientS.MaxWindowSize()))

	err := sc.readLoop()
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameGoAway, h2Err.frameType)

	fr := <-sc.writer
	require.Equal(t, FrameSettings, fr.Type())
	require.True(t, fr.Body().(*Settings).IsAck())
	ReleaseFrameHeader(fr)

	fr = <-sc.writer
	require.Equal(t, FrameGoAway, fr.Type())
	require.Equal(t, ProtocolError, fr.Body().(*GoAway).Code())
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

func TestStreamWindowUpdateOverflowResetsStream(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		logger: log.New(io.Discard, "", 0),
	}

	strm := NewStream(1, int32(defaultWindowSize), int32(defaultWindowSize))
	strm.SetState(StreamStateOpen)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.SetIncrement(maxWindowIncrement)

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	fr.SetBody(wu)
	fr.length = 4

	err := sc.handleFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, FlowControlError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)
	require.EqualValues(t, maxWindowIncrement, atomic.LoadInt64(&strm.sendWindow))

	sc.writeError(strm, err)
	out := <-sc.writer
	require.Equal(t, FrameResetStream, out.Type())
	require.Equal(t, FlowControlError, out.Body().(*RstStream).Code())

	ReleaseFrameHeader(out)
	ReleaseFrameHeader(fr)
}

func TestStreamWindowUpdateZeroSendsProtocolReset(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 1),
		logger: log.New(io.Discard, "", 0),
	}

	strm := NewStream(1, int32(defaultWindowSize), int32(defaultWindowSize))
	strm.SetState(StreamStateOpen)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.increment = 0

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())
	fr.SetBody(wu)
	fr.length = 4

	err := sc.handleFrame(strm, fr)
	require.Error(t, err)
	var h2Err Error
	require.ErrorAs(t, err, &h2Err)
	require.Equal(t, ProtocolError, h2Err.Code())
	require.Equal(t, FrameResetStream, h2Err.frameType)

	sc.writeError(strm, err)
	out := <-sc.writer
	require.Equal(t, FrameResetStream, out.Type())
	require.Equal(t, ProtocolError, out.Body().(*RstStream).Code())

	ReleaseFrameHeader(out)
	ReleaseFrameHeader(fr)
}

func TestQueueDataRespectsInitialWindowChanges(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 4),
		logger: log.New(io.Discard, "", 0),
	}
	sc.clientS.Reset()
	// Initialize connection window to allow sending
	atomic.StoreInt64(&sc.clientWindow, int64(defaultWindowSize))

	strm := NewStream(1, int32(defaultWindowSize), 3)
	strm.SetState(StreamStateOpen)

	body := []byte("hello world")
	sc.queueData(strm, body, true)

	first := <-sc.writer
	data := first.Body().(*Data)
	require.Equal(t, 3, len(data.Data()))
	require.False(t, data.EndStream())
	ReleaseFrameHeader(first)
	require.EqualValues(t, 0, atomic.LoadInt64(&strm.sendWindow))

	atomic.AddInt64(&strm.sendWindow, -1)

	_, err := addAndClampWindow(&strm.sendWindow, 2)
	require.NoError(t, err)
	sc.flushPendingData(strm)

	next := <-sc.writer
	nextData := next.Body().(*Data)
	require.Equal(t, 1, len(nextData.Data()))
	ReleaseFrameHeader(next)

	strm.pendingMu.Lock()
	pendingLen := len(strm.pendingData)
	strm.pendingMu.Unlock()
	require.Equal(t, len(body)-4, pendingLen)
}

func TestConnectionWindowLimitsDataAcrossStreams(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 8),
		logger: log.New(io.Discard, "", 0),
	}
	atomic.StoreInt64(&sc.clientWindow, 5)

	strm1 := NewStream(1, int32(defaultWindowSize), int32(defaultWindowSize))
	strm1.SetState(StreamStateOpen)
	strm2 := NewStream(3, int32(defaultWindowSize), int32(defaultWindowSize))
	strm2.SetState(StreamStateOpen)

	sc.addActiveStream(strm1)
	sc.addActiveStream(strm2)

	sc.queueData(strm1, []byte("abc"), false)
	sc.queueData(strm2, []byte("def"), true)

	require.EqualValues(t, 0, atomic.LoadInt64(&sc.clientWindow))

	first := <-sc.writer
	require.Equal(t, FrameData, first.Type())
	require.Equal(t, strm1.ID(), first.Stream())
	firstData := first.Body().(*Data)
	require.Equal(t, "abc", string(firstData.Data()))
	require.False(t, firstData.EndStream())
	ReleaseFrameHeader(first)

	second := <-sc.writer
	require.Equal(t, FrameData, second.Type())
	require.Equal(t, strm2.ID(), second.Stream())
	secondData := second.Body().(*Data)
	require.Equal(t, "de", string(secondData.Data()))
	require.False(t, secondData.EndStream())
	ReleaseFrameHeader(second)

	strm2.pendingMu.Lock()
	require.Equal(t, 1, len(strm2.pendingData))
	require.True(t, strm2.pendingDataEndStream)
	strm2.pendingMu.Unlock()

	_, err := addAndClampWindow(&sc.clientWindow, 3)
	require.NoError(t, err)

	sc.forEachActiveStream(func(strm *Stream) {
		sc.flushPendingData(strm)
	})

	third := <-sc.writer
	require.Equal(t, FrameData, third.Type())
	require.Equal(t, strm2.ID(), third.Stream())
	thirdData := third.Body().(*Data)
	require.Equal(t, "f", string(thirdData.Data()))
	require.True(t, thirdData.EndStream())
	ReleaseFrameHeader(third)

	require.EqualValues(t, 2, atomic.LoadInt64(&sc.clientWindow))
}

func TestConnectionWindowQueuesUntilUpdate(t *testing.T) {
	sc := &serverConn{
		writer: make(chan *FrameHeader, 4),
		logger: log.New(io.Discard, "", 0),
	}
	atomic.StoreInt64(&sc.clientWindow, 0)
	atomic.StoreInt64(&sc.initialClientWindow, int64(defaultWindowSize))

	strm := NewStream(1, int32(defaultWindowSize), int32(defaultWindowSize))
	strm.SetState(StreamStateOpen)
	sc.addActiveStream(strm)

	sc.queueData(strm, []byte("hello"), true)

	require.Equal(t, 0, len(sc.writer))

	strm.pendingMu.Lock()
	require.Equal(t, 5, len(strm.pendingData))
	require.True(t, strm.pendingDataEndStream)
	strm.pendingMu.Unlock()

	_, err := addAndClampWindow(&sc.clientWindow, 5)
	require.NoError(t, err)

	sc.forEachActiveStream(func(s *Stream) {
		sc.flushPendingData(s)
	})

	fr := <-sc.writer
	require.Equal(t, FrameData, fr.Type())
	data := fr.Body().(*Data)
	require.Equal(t, "hello", string(data.Data()))
	require.True(t, data.EndStream())
	ReleaseFrameHeader(fr)

	require.EqualValues(t, 0, atomic.LoadInt64(&sc.clientWindow))
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

	sc.closeWriter()
	<-writerDone
}

func TestHandleStreamsRespectsMaxConcurrentStreams(t *testing.T) {
	sc := &serverConn{
		reader:          make(chan *FrameHeader, 8),
		writer:          make(chan *FrameHeader, 8),
		maxRequestTimer: time.NewTimer(time.Hour),
		maxRequestTime:  time.Hour,
		closer:          make(chan struct{}),
		logger:          log.New(io.Discard, "", 0),
		h:               func(ctx *fasthttp.RequestCtx) {},
	}
	sc.st.Reset()
	sc.st.SetMaxConcurrentStreams(1)
	sc.clientS.Reset()
	atomic.StoreInt64(&sc.clientWindow, int64(sc.clientS.MaxWindowSize()))
	sc.dec.Reset()

	streamsDone := make(chan struct{})
	go func() {
		sc.handleStreams()
		close(streamsDone)
	}()

	headers := [][2]string{
		{":method", "GET"},
		{":authority", "localhost"},
		{":scheme", "https"},
		{":path", "/"},
	}

	sc.reader <- buildHeadersFrame(t, 1, headers)
	sc.reader <- buildHeadersFrame(t, 3, headers)

	var rst *FrameHeader
	select {
	case rst = <-sc.writer:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for stream reset")
	}

	require.Equal(t, FrameResetStream, rst.Type())
	require.Equal(t, RefusedStreamError, rst.Body().(*RstStream).Code())
	ReleaseFrameHeader(rst)

	close(sc.closer)
	close(sc.reader)
	<-streamsDone
	sc.closeWriter()

	require.Equal(t, uint32(3), sc.lastID)

	for fr := range sc.writer {
		ReleaseFrameHeader(fr)
	}
	sc.maxRequestTimer.Stop()
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
	sc.closeWriter()
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
	data := appendEmptySettingsFrame(append(header, payload...))

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

func TestConnectionErrorClosesAfterGoAway(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {},
		},
	}
	s.cnf.defaults()

	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()

	done := make(chan struct{})
	go func() {
		_ = s.ServeConn(serverSide)
		close(done)
	}()

	serverData := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(clientSide)
		serverData <- data
		close(serverData)
	}()

	require.NoError(t, WritePreface(clientSide))

	invalidPing := append([]byte{
		0x0, 0x0, 0x8,
		byte(FramePing), 0x0,
		0x0, 0x0, 0x0, 0x3,
	}, make([]byte, 8)...)
	_, err := clientSide.Write(invalidPing)
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for server to close connection")
	}

	frames := <-serverData
	reader := bufio.NewReader(bytes.NewReader(frames))
	foundGoAway := false
	var seenTypes []FrameType
	for {
		fr, frErr := ReadFrameFromWithSize(reader, defaultDataFrameSize)
		if frErr != nil {
			require.Equal(t, io.EOF, frErr)
			break
		}

		seenTypes = append(seenTypes, fr.Type())
		if fr.Type() == FrameGoAway {
			foundGoAway = true
		}
		ReleaseFrameHeader(fr)
	}

	require.True(t, foundGoAway, "expected GOAWAY frame before connection closed (frames=%v)", seenTypes)
}

func TestUnknownFrameDuringHeaderBlockCausesConnectionError(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {},
		},
	}
	s.cnf.defaults()

	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()

	go func() {
		_ = s.ServeConn(serverSide)
	}()

	serverData := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(clientSide)
		serverData <- data
		close(serverData)
	}()

	require.NoError(t, WritePreface(clientSide))

	bw := bufio.NewWriter(clientSide)
	writeEmptySettingsFrame(t, bw)
	headers := buildHeadersFrameWithOptions(t, 1, [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":authority", "localhost"},
		{":path", "/"},
	}, false, false)
	_, err := headers.WriteTo(bw)
	require.NoError(t, err)
	ReleaseFrameHeader(headers)
	require.NoError(t, bw.Flush())

	_, err = bw.Write([]byte("\x00\x00\x08\x16\x00\x00\x00\x00\x00"))
	require.NoError(t, err)
	_, err = bw.Write([]byte("\x00\x00\x00\x00\x00\x00\x00\x00"))
	require.NoError(t, err)
	require.NoError(t, bw.Flush())

	var frames []byte
	select {
	case frames = <-serverData:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for connection error")
	}
	reader := bufio.NewReader(bytes.NewReader(frames))
	var goAwayCodes []ErrorCode
	for {
		fr, frErr := ReadFrameFromWithSize(reader, defaultDataFrameSize)
		if frErr != nil {
			require.Equal(t, io.EOF, frErr)
			break
		}

		if fr.Type() == FrameGoAway {
			goAwayCodes = append(goAwayCodes, fr.Body().(*GoAway).Code())
		}
		ReleaseFrameHeader(fr)
	}

	require.Contains(t, goAwayCodes, ProtocolError, "expected GOAWAY with PROTOCOL_ERROR")
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

func TestWriteLoopErrorClosesConnection(t *testing.T) {
	srv, cli := net.Pipe()
	t.Cleanup(func() {
		_ = cli.Close()
	})

	conn := &writeFailConn{
		Conn:     srv,
		writeErr: errors.New("write failure"),
	}

	sc := &serverConn{
		c:               conn,
		br:              bufio.NewReader(conn),
		bw:              bufio.NewWriter(conn),
		writer:          make(chan *FrameHeader, 1),
		reader:          make(chan *FrameHeader, 1),
		maxRequestTimer: time.NewTimer(time.Hour),
		maxRequestTime:  time.Hour,
		closer:          make(chan struct{}),
		logger:          log.New(io.Discard, "", 0),
	}
	sc.st.Reset()
	sc.clientS.Reset()
	sc.dec.Reset()

	streamsDone := make(chan struct{})
	go func() {
		sc.handleStreams()
		sc.stopPingTimer()
		sc.closeWriter()
		close(streamsDone)
	}()

	readDone := make(chan error, 1)
	go func() {
		readDone <- sc.readLoop()
	}()

	writeDone := make(chan struct{})
	go func() {
		sc.writeLoop()
		close(writeDone)
	}()

	fr := AcquireFrameHeader()
	data := AcquireFrame(FrameData).(*Data)
	data.SetData([]byte("payload"))
	data.SetEndStream(true)
	fr.SetStream(1)
	fr.SetBody(data)
	sc.writer <- fr

	// Allow additional writes after the failure without panicking.
	fr2 := AcquireFrameHeader()
	fr2.SetStream(3)
	fr2.SetBody(AcquireFrame(FrameResetStream))
	select {
	case sc.writer <- fr2:
	case <-time.After(time.Second):
		t.Fatal("timed out sending frame after write failure")
	}

	require.Eventually(t, func() bool {
		select {
		case <-writeDone:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		select {
		case <-streamsDone:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		select {
		case <-sc.closer:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return conn.closed.Load()
	}, time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		select {
		case <-readDone:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	select {
	case _, ok := <-sc.writer:
		require.False(t, ok)
	default:
		t.Fatal("writer channel not closed")
	}

	sc.maxRequestTimer.Stop()
}
