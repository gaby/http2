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

	strm := NewStream(1, 10)
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

func TestStreamWriteHelpers(t *testing.T) {
	writer := make(chan *FrameHeader, 4)
	strm := NewStream(3, 100)

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
	s1 := NewStream(1, 0)
	s1.origType = FrameHeaders
	s2 := NewStream(2, 0)
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

	strm := NewStream(1, int32(defaultWindowSize))
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

func TestServerConnFrameTooLargeSendsGoAway(t *testing.T) {
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
	require.Equal(t, FrameGoAway, fr.Type())
	require.Equal(t, FrameSizeError, fr.Body().(*GoAway).Code())
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

func TestServerConnResponseHeadersConcurrentSettings(t *testing.T) {
	const iterations = 64

	sc := &serverConn{
		writer: make(chan *FrameHeader, iterations),
		logger: log.New(io.Discard, "", 0),
	}

	sc.clientS.Reset()
	sc.enc.Reset()

	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(res)
	res.Header.SetStatusCode(200)
	res.Header.Set("X-Test", "value")

	writerDone := make(chan struct{})
	go func() {
		for fr := range sc.writer {
			ReleaseFrameHeader(fr)
		}
		close(writerDone)
	}()

	start := make(chan struct{})
	settingsDone := make(chan struct{})

	go func() {
		<-start
		st := &Settings{}
		st.Reset()
		for i := 0; i < iterations; i++ {
			st.SetHeaderTableSize(defaultHeaderTableSize + uint32(i))
			sc.handleSettings(st)
		}
		close(sc.writer)
		close(settingsDone)
	}()

	close(start)
	for i := 0; i < iterations; i++ {
		h := AcquireFrame(FrameHeaders).(*Headers)
		sc.appendResponseHeaders(h, res)
		ReleaseFrame(h)
	}

	<-settingsDone
	<-writerDone
}
