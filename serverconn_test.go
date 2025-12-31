package http2

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServerConnPingAndErrors(t *testing.T) {
	writer := make(chan *FrameHeader, 4)
	sc := &serverConn{
		writer: writer,
		clock:  realClock{},
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
		clock:           realClock{},
		maxRequestTimer: (realClock{}).NewTimer(time.Hour),
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

func TestServerConnRequestTimerUsesClock(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	sc := &serverConn{
		clock:           clock,
		maxRequestTimer: clock.NewTimer(time.Hour),
		maxRequestTime:  time.Second,
		logger:          log.New(io.Discard, "", 0),
	}

	sc.resetMaxRequestTimer(sc.maxRequestTime)
	require.LessOrEqual(t, clock.NextFireIn(), time.Second)

	clock.Advance(time.Second)

	select {
	case <-sc.maxRequestTimer.C():
	default:
		require.Fail(t, "maxRequestTimer did not fire after advancing")
	}

	sc.close()
	require.Equal(t, 0, clock.PendingTimers())
}

func TestServerConnIdleTimeoutWithFakeClock(t *testing.T) {
	clock := NewFakeClock(time.Unix(0, 0))
	sc := &serverConn{
		writer:      make(chan *FrameHeader, 1),
		clock:       clock,
		maxIdleTime: time.Second,
		closer:      make(chan struct{}),
		logger:      log.New(io.Discard, "", 0),
	}

	sc.maxIdleTimer = clock.AfterFunc(sc.maxIdleTime, sc.closeIdleConn)

	clock.Advance(time.Second)

	var goaway *FrameHeader
	require.Eventually(t, func() bool {
		select {
		case goaway = <-sc.writer:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond*10)
	require.Equal(t, FrameGoAway, goaway.Type())
	ReleaseFrameHeader(goaway)

	require.Eventually(t, func() bool {
		select {
		case <-sc.closer:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond*10, "closer channel not closed")
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
		br:      bufio.NewReader(bytes.NewReader(data)),
		clientS: Settings{},
		writer:  make(chan *FrameHeader, 1),
		logger:  log.New(io.Discard, "", 0),
		clock:   realClock{},
	}
	sc.clientS.Reset()
	sc.clientS.SetMaxFrameSize(maxSize)

	err := sc.readLoop()
	require.ErrorIs(t, err, ErrPayloadExceeds)

	fr := <-sc.writer
	require.Equal(t, FrameGoAway, fr.Type())
	require.Equal(t, FrameSizeError, fr.Body().(*GoAway).Code())
	ReleaseFrameHeader(fr)
}
