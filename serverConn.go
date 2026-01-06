package http2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

type connState int32

const (
	connStateOpen connState = iota
	connStateClosed
)

type serverConn struct {
	c net.Conn
	h fasthttp.RequestHandler

	br *bufio.Reader
	bw *bufio.Writer

	encMu sync.Mutex // guards enc
	enc   HPACK
	dec   HPACK
	// clientSMu guards access to clientS
	clientSMu sync.Mutex

	// last valid ID used as a reference for new IDs
	lastID uint32

	// client's window
	// should be int64 because the user can try to overflow it
	clientWindow int64
	// initial window advertised by the client for new streams
	initialClientWindow int64
	// client's max frame size
	clientMaxFrameSize int64

	// our values
	maxWindow     int32
	currentWindow int32
	writer        chan *FrameHeader
	reader        chan *FrameHeader
	writerMu      sync.RWMutex
	writerClosed  atomic.Bool

	streamsMu sync.Mutex
	streams   map[uint32]*Stream

	state connState
	// connErr signals that a connection-level error has been triggered and the
	// socket should be closed after sending the GOAWAY frame.
	connErr atomic.Bool
	// closing signals that the connection should be closed soon, either due to
	// a connection-level error or because the server is shutting down the
	// connection (for example, after an idle timeout).
	closing atomic.Bool
	// closeRef stores the last stream that was valid before sending a GOAWAY.
	// Thus, the number stored in closeRef is used to complete all the requests that were sent before
	// to gracefully close the connection with a GOAWAY.
	closeRef uint32

	// maxRequestTime is the max time of a request over one single stream
	maxRequestTime time.Duration
	pingInterval   time.Duration
	// maxIdleTime is the max time a client can be connected without sending any REQUEST.
	// As highlighted, PING/PONG frames are completely excluded.
	//
	// Therefore, a client that didn't send a request for more than `maxIdleTime` will see it's connection closed.
	maxIdleTime time.Duration

	st      Settings
	clientS Settings

	// pingTimer
	pingTimer       *time.Timer
	maxRequestTimer *time.Timer
	maxIdleTimer    *time.Timer
	pingTimerMu     sync.Mutex
	timerMu         sync.Mutex

	closer chan struct{}

	debug  bool
	logger fasthttp.Logger

	closerOnce      sync.Once
	writerCloseOnce sync.Once
}

// defaultEnqueueTimeout bounds how long a sender will wait when the writer
// channel is full. A generous timeout avoids dropping frames under normal
// load, while still letting shutdown make progress if the writer loop is
// wedged.
const defaultEnqueueTimeout = 2 * time.Second

func (sc *serverConn) closeIdleConn() {
	if sc.isClosed() || sc.closing.Load() {
		return
	}

	const idleCloseGrace = 200 * time.Millisecond

	// Try to send GOAWAY but bound the wait so the idle timeout always closes.
	// Using a short timeout keeps the timer goroutine from hanging if the writer
	// channel is full or closed.
	sent := sc.writeGoAwayWithTimeout(0, NoError, "connection has been idle for a long time", 100*time.Millisecond)
	if sc.debug && !sent {
		sc.logger.Printf("Connection is idle. Failed to send GOAWAY before closing\n")
	}
	if sc.debug {
		sc.logger.Printf("Connection is idle. Closing\n")
	}
	sc.closeCloser()
	if sent {
		time.AfterFunc(idleCloseGrace, sc.signalConnClose)
	} else {
		sc.signalConnClose()
	}
}

func (sc *serverConn) signalConnError() {
	if sc.connErr.CompareAndSwap(false, true) {
		sc.signalConnClose()
	}
}

func (sc *serverConn) signalConnClose() {
	if sc.closing.CompareAndSwap(false, true) {
		if sc.c != nil {
			_ = sc.c.SetReadDeadline(time.Now())
			go func(c net.Conn) {
				time.Sleep(50 * time.Millisecond)
				_ = c.Close()
			}(sc.c)
		}
	}
}

func (sc *serverConn) closeCloser() {
	if sc.closer == nil {
		return
	}
	sc.closerOnce.Do(func() {
		close(sc.closer)
	})
}

func (sc *serverConn) closeWriter() {
	if sc.writer == nil {
		return
	}
	sc.writerCloseOnce.Do(func() {
		sc.writerMu.Lock()
		sc.writerClosed.Store(true)
		defer func() { _ = recover() }()
		close(sc.writer)
		sc.writerMu.Unlock()
	})
}

func (sc *serverConn) enqueueFrame(fr *FrameHeader) {
	if sc.enqueueFrameInternal(fr, defaultEnqueueTimeout) {
		return
	}

	ReleaseFrameHeader(fr)
}

func (sc *serverConn) enqueueFrameInternal(fr *FrameHeader, d time.Duration) bool {
	if sc.writer == nil {
		return false
	}

	if sc.closing.Load() {
		return false
	}

	if sc.closer != nil {
		select {
		case <-sc.closer:
			return false
		default:
		}
	}

	sc.writerMu.RLock()
	defer sc.writerMu.RUnlock()

	if sc.writerClosed.Load() {
		return false
	}

	if d <= 0 {
		d = defaultEnqueueTimeout
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	defer func() {
		if r := recover(); r != nil {
			if sc.debug {
				sc.logger.Printf("enqueueFrame: writer closed, dropping frame: %v\n", r)
			}
		}
	}()

	select {
	case sc.writer <- fr:
		return true
	case <-timer.C:
		return false
	}
}

func (sc *serverConn) Handshake() error {
	return Handshake(false, sc.bw, &sc.st, sc.maxWindow)
}

func (sc *serverConn) Serve() error {
	sc.closer = make(chan struct{}, 1)
	sc.timerMu.Lock()
	sc.maxRequestTimer = time.NewTimer(0)
	sc.clientSMu.Lock()
	initWin := sc.clientS.MaxWindowSize()
	sc.clientSMu.Unlock()
	if initWin == 0 {
		initWin = defaultWindowSize
	}
	atomic.StoreInt64(&sc.clientWindow, int64(initWin))
	atomic.StoreInt64(&sc.initialClientWindow, int64(initWin))
	atomic.StoreInt64(&sc.clientMaxFrameSize, int64(defaultDataFrameSize))

	if sc.maxIdleTime > 0 {
		sc.maxIdleTimer = time.AfterFunc(sc.maxIdleTime, sc.closeIdleConn)
	}
	sc.timerMu.Unlock()

	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("Serve panicked: %s:\n%s\n", err, debug.Stack())
		}
	}()

	writerDone := make(chan struct{})
	go func() {
		// defer closing the connection in the writeLoop in case the writeLoop panics
		defer func() {
			_ = sc.c.Close()
		}()

		sc.writeLoop()
		close(writerDone)
	}()

	go func() {
		sc.handleStreams()
		// Fix #55: The pingTimer fired while we were closing the connection.
		sc.stopPingTimer()
		// close the writer here to ensure that no pending requests
		// are writing to a closed channel
		sc.closeWriter()
	}()

	var err error

	// unset any deadline
	if err = sc.c.SetWriteDeadline(time.Time{}); err == nil {
		err = sc.c.SetReadDeadline(time.Time{})
	}
	if err != nil {
		return err
	}

	err = sc.readLoop()
	if errors.Is(err, io.EOF) {
		err = nil
	}

	close(sc.reader)
	sc.close()
	<-writerDone

	return err
}

func (sc *serverConn) close() {
	atomic.StoreInt32((*int32)(&sc.state), int32(connStateClosed))

	sc.stopPingTimer()

	sc.timerMu.Lock()
	if sc.maxIdleTimer != nil {
		sc.maxIdleTimer.Stop()
	}

	if sc.maxRequestTimer != nil {
		sc.maxRequestTimer.Stop()
	}
	sc.timerMu.Unlock()
}

func (sc *serverConn) resetMaxRequestTimer(d time.Duration) {
	sc.timerMu.Lock()
	if sc.maxRequestTimer != nil {
		sc.maxRequestTimer.Reset(d)
	}
	sc.timerMu.Unlock()
}

func (sc *serverConn) resetMaxIdleTimer() {
	sc.timerMu.Lock()
	if sc.maxIdleTimer != nil {
		sc.maxIdleTimer.Reset(sc.maxIdleTime)
	}
	sc.timerMu.Unlock()
}

func (sc *serverConn) stopPingTimer() {
	sc.pingTimerMu.Lock()
	if sc.pingTimer != nil {
		sc.pingTimer.Stop()
		sc.pingTimer = nil
	}
	sc.pingTimerMu.Unlock()
}

func (sc *serverConn) isClosed() bool {
	return atomic.LoadInt32((*int32)(&sc.state)) == int32(connStateClosed)
}

func (sc *serverConn) handlePing(ping *Ping) {
	fr := AcquireFrameHeader()
	ping.SetAck(true)
	fr.SetBody(ping)

	sc.enqueueFrame(fr)
}

func (sc *serverConn) writePing() {
	if atomic.LoadInt32((*int32)(&sc.state)) == int32(connStateClosed) {
		return
	}

	fr := AcquireFrameHeader()

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	defer func() {
		if r := recover(); r != nil {
			ReleaseFrameHeader(fr)
		}
	}()

	sc.enqueueFrame(fr)
}

func (sc *serverConn) checkFrameWithStream(fr *FrameHeader) error {
	if fr.Stream()&1 == 0 {
		return NewGoAwayError(ProtocolError, "invalid stream id")
	}

	switch fr.Type() {
	case FramePing:
		return NewGoAwayError(ProtocolError, "ping is carrying a stream id")
	case FramePushPromise:
		return NewGoAwayError(ProtocolError, "clients can't send push_promise frames")
	}

	return nil
}

func (sc *serverConn) readLoop() (err error) {
	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("readLoop panicked: %s\n%s\n", err, debug.Stack())
		}
	}()

	var fr *FrameHeader
	openHeaderBlocks := make(map[uint32]struct{})
	firstFrame := true

	invalidFirstFrame := func(fr *FrameHeader) error {
		sc.writeGoAway(0, ProtocolError, "invalid first frame")
		sc.signalConnError()
		if fr != nil {
			ReleaseFrameHeader(fr)
		}
		return NewGoAwayError(ProtocolError, "invalid first frame")
	}

	for err == nil {
		if sc.connErr.Load() || sc.closing.Load() {
			break
		}

		fr, err = ReadFrameFromWithSize(sc.br, sc.st.MaxFrameSize())
		if err != nil {
			if errors.Is(err, ErrUnknownFrameType) {
				if firstFrame {
					err = invalidFirstFrame(fr)
					break
				}
				if fr != nil {
					if len(openHeaderBlocks) > 0 {
						sc.writeGoAway(0, ProtocolError, "invalid frame")
						sc.signalConnError()
						ReleaseFrameHeader(fr)
						err = NewGoAwayError(ProtocolError, "invalid frame")
						break
					}
					ReleaseFrameHeader(fr)
				}
				err = nil
				continue
			}

			if errors.Is(err, ErrPayloadExceeds) {
				switch {
				case fr == nil,
					fr.Stream() == 0,
					fr.Type() == FrameSettings,
					fr.Type() == FrameHeaders,
					fr.Type() == FramePushPromise,
					fr.Type() == FrameContinuation:
					sc.writeGoAway(0, FrameSizeError, "frame size exceeds maximum")
				default:
					sc.writeReset(fr.Stream(), FrameSizeError)
				}

				if fr != nil {
					ReleaseFrameHeader(fr)
				}

				if fr == nil || fr.Stream() == 0 {
					sc.signalConnError()
				}
			}

			var h2Err Error
			if errors.As(err, &h2Err) && isConnectionError(h2Err) {
				sc.writeError(nil, h2Err)
				sc.signalConnError()
			}

			break
		}

		if firstFrame {
			firstFrame = false
			if fr.Stream() != 0 || fr.Type() != FrameSettings {
				err = invalidFirstFrame(fr)
				break
			}

			st, ok := fr.Body().(*Settings)
			if !ok || st.IsAck() {
				err = invalidFirstFrame(fr)
				break
			}
		}

		switch fr.Type() {
		case FrameHeaders, FrameContinuation:
			if fr.Flags().Has(FlagEndHeaders) {
				delete(openHeaderBlocks, fr.Stream())
			} else {
				openHeaderBlocks[fr.Stream()] = struct{}{}
			}
		case FrameResetStream:
			delete(openHeaderBlocks, fr.Stream())
		}

		if fr.Stream() != 0 {
			cerr := sc.checkFrameWithStream(fr)
			if cerr != nil {
				sc.writeError(nil, cerr)
				if isConnectionError(cerr) {
					sc.signalConnError()
					sc.closeWriter()
					sc.closeCloser()
					if sc.c != nil {
						_ = sc.c.Close()
					}
					ReleaseFrameHeader(fr)
					err = cerr
					break
				}

				ReleaseFrameHeader(fr)
			} else {
				if sc.connErr.Load() {
					ReleaseFrameHeader(fr)
					break
				}
				sc.reader <- fr
			}

			continue
		}

		// handle 'anonymous' frames (frames without stream_id)
		var frameErr error
		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if it has ack, just ignore
				sc.handleSettings(st)
			}
		case FrameWindowUpdate:
			win := int64(fr.Body().(*WindowUpdate).Increment())
			if incErr := validateWindowIncrement(win); incErr != nil {
				code := FlowControlError
				if errors.Is(incErr, errWindowIncrementZero) {
					code = ProtocolError
				}

				msg := windowUpdateErrorMessage(incErr)
				sc.writeGoAway(0, code, msg)
				frameErr = NewGoAwayError(code, msg)
			} else if _, winErr := addAndClampWindow(&sc.clientWindow, win); winErr != nil {
				msg := windowUpdateErrorMessage(winErr)
				sc.writeGoAway(0, FlowControlError, msg)
				frameErr = NewGoAwayError(FlowControlError, msg)
			} else {
				sc.forEachActiveStream(func(strm *Stream) {
					sc.flushPendingData(strm)
				})
			}
		case FramePing:
			ping := fr.Body().(*Ping)
			if !ping.IsAck() {
				sc.handlePing(ping)
			}
		case FrameGoAway:
			ga := fr.Body().(*GoAway)
			if ga.Code() == NoError {
				err = io.EOF
			} else {
				err = fmt.Errorf("goaway: %s: %s", ga.Code(), ga.Data())
			}
		default:
			sc.writeGoAway(0, ProtocolError, "invalid frame")
			frameErr = NewGoAwayError(ProtocolError, "invalid frame")
		}

		if frameErr != nil {
			if isConnectionError(frameErr) {
				// Delay signaling close very slightly so the GOAWAY has a chance
				// to flush before we tear down the socket.
				go func() {
					time.Sleep(20 * time.Millisecond)
					sc.signalConnError()
				}()
			}
			err = frameErr
		}

		ReleaseFrameHeader(fr)
	}

	if sc.closing.Load() && !sc.connErr.Load() {
		err = nil
	}

	return
}

// handleStreams handles everything related to the streams
// and the HPACK table is accessed synchronously.
func (sc *serverConn) handleStreams() {
	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("handleStreams panicked: %s\n%s\n", err, debug.Stack())
		}
	}()

	var strms Streams
	var reqTimerArmed bool
	var openStreams int

	closedStrms := make(map[uint32]struct{})

	markClosedStream := func(id uint32, origType FrameType) {
		closedStrms[id] = struct{}{}

		if origType == FrameHeaders && id > sc.lastID {
			sc.lastID = id
		}
	}

	closeStream := func(strm *Stream) {
		if strm.origType == FrameHeaders {
			openStreams--
		}

		strmID := strm.ID()
		sc.deleteActiveStream(strmID)

		closedStrms[strm.ID()] = struct{}{}
		strms.Del(strm.ID())

		ctxPool.Put(strm.ctx)

		if sc.debug {
			sc.logger.Printf("Stream destroyed %d. Open streams: %d\n", strmID, openStreams)
		}
	}

loop:
	for {
		select {
		case <-sc.closer:
			break loop
		case <-sc.maxRequestTimer.C:
			reqTimerArmed = false

			deleteUntil := 0
			for _, strm := range strms {
				// the request is due if the startedAt time + maxRequestTime is in the past
				isDue := time.Now().After(
					strm.startedAt.Add(sc.maxRequestTime))
				if !isDue {
					break
				}

				deleteUntil++
			}

			for deleteUntil > 0 {
				strm := strms[0]

				if sc.debug {
					sc.logger.Printf("Stream timed out: %d\n", strm.ID())
				}
				sc.writeReset(strm.ID(), StreamCanceled)

				// set the state to closed in case it comes back to life later
				strm.SetState(StreamStateClosed)
				closeStream(strm)

				deleteUntil--
			}

			if len(strms) != 0 && sc.maxRequestTime > 0 {
				// the first in the stream list might have started with a PushPromise
				strm := strms.GetFirstOf(FrameHeaders)
				if strm != nil {
					reqTimerArmed = true
					// try to arm the timer
					when := strm.startedAt.Add(sc.maxRequestTime).Sub(time.Now())
					// if the time is negative or zero it triggers imm
					sc.resetMaxRequestTimer(when)

					if sc.debug {
						sc.logger.Printf("Next request will timeout in %f seconds\n", when.Seconds())
					}
				}
			}
		case fr, ok := <-sc.reader:
			if !ok {
				return
			}

			// Ensure idle connections close even if the idle timer is lost; keep
			// a read deadline aligned with maxIdleTime after each client frame.
			if sc.maxIdleTime > 0 && sc.c != nil {
				_ = sc.c.SetReadDeadline(time.Now().Add(sc.maxIdleTime))
			}

			isClosing := atomic.LoadInt32((*int32)(&sc.state)) == int32(connStateClosed)

			var strm *Stream
			if fr.Stream() <= sc.lastID {
				strm = strms.Search(fr.Stream())
			}

			if fr.Body() == nil {
				pendingHeaders := strm != nil && !strm.headersFinished
				if !pendingHeaders {
					for _, s := range strms {
						if s.origType == FrameHeaders && !s.headersFinished {
							pendingHeaders = true
							break
						}
					}
				}

				if pendingHeaders {
					sc.writeGoAway(fr.Stream(), ProtocolError, "frame on incomplete header block")
					sc.signalConnError()
					ReleaseFrameHeader(fr)
					break loop
				}

				ReleaseFrameHeader(fr)
				continue
			}

			if strm == nil {
				// if the stream doesn't exist, create it

				if fr.Type() == FrameResetStream {
					// only send go away on idle stream not on an already-closed stream
					if _, ok := closedStrms[fr.Stream()]; !ok {
						sc.writeGoAway(fr.Stream(), ProtocolError, "RST_STREAM on idle stream")
					}

					continue
				}

				if _, ok := closedStrms[fr.Stream()]; ok {
					if fr.Type() != FramePriority {
						sc.writeGoAway(fr.Stream(), StreamClosedError, "frame on closed stream")
					}

					continue
				}

				// if the client has more open streams than the maximum allowed OR
				//   the connection is closing, then refuse the stream
				if openStreams >= int(sc.st.maxStreams) || isClosing {
					if sc.debug {
						if isClosing {
							sc.logger.Printf("Closing the connection. Rejecting stream %d\n", fr.Stream())
						} else {
							sc.logger.Printf("Max open streams reached: %d >= %d\n",
								openStreams, sc.st.maxStreams)
						}
					}

					sc.writeReset(fr.Stream(), RefusedStreamError)
					markClosedStream(fr.Stream(), fr.Type())

					continue
				}

				if fr.Stream() < sc.lastID {
					sc.writeGoAway(fr.Stream(), ProtocolError, "stream ID is lower than the latest")
					continue
				}

				recvWindow := int32(sc.st.MaxWindowSize())

				// Hold streamsMu while reading initialClientWindow and adding stream
				// to prevent race with handleSettings updating it.
				// Note: SETTINGS frames are now processed through the channel in order,
				// so initialClientWindow always reflects the correct value including 0
				// if the client explicitly set it to 0.
				sc.streamsMu.Lock()
				sendWindow := int32(atomic.LoadInt64(&sc.initialClientWindow))
				strm = NewStream(fr.Stream(), recvWindow, sendWindow)
				strms = append(strms, strm)
				if sc.streams == nil {
					sc.streams = make(map[uint32]*Stream)
				}
				sc.streams[strm.ID()] = strm
				sc.streamsMu.Unlock()

				// RFC(5.1.1):
				//
				// The identifier of a newly established stream MUST be numerically
				// greater than all streams that the initiating endpoint has opened
				// or reserved. This governs streams that are opened using a
				// HEADERS frame and streams that are reserved using PUSH_PROMISE.
				if fr.Type() == FrameHeaders {
					openStreams++
					sc.lastID = fr.Stream()
				}

				sc.createStream(sc.c, fr.Type(), strm)

				if sc.debug {
					sc.logger.Printf("Stream %d created. Open streams: %d\n", strm.ID(), openStreams)
				}

				if !reqTimerArmed && sc.maxRequestTime > 0 {
					reqTimerArmed = true
					sc.resetMaxRequestTimer(sc.maxRequestTime)

					if sc.debug {
						sc.logger.Printf("Next request will timeout in %f seconds\n", sc.maxRequestTime.Seconds())
					}
				}
			}

			// if we have more than one stream (this one newly created) check if the previous finished sending the headers
			if fr.Type() == FrameHeaders {
				nstrm := strms.getPrevious(FrameHeaders)
				if nstrm != nil && !nstrm.headersFinished {
					sc.writeError(nstrm, NewGoAwayError(ProtocolError, "previous stream headers not ended"))
					continue
				}

				for len(strms) != 0 {
					nstrm := strms[0]
					// RFC(5.1.1):
					//
					// The first use of a new stream identifier implicitly
					// closes all streams in the "idle" state that might
					// have been initiated by that peer with a lower-valued stream identifier
					if nstrm.ID() < strm.ID() &&
						nstrm.State() == StreamStateIdle &&
						nstrm.origType == FrameHeaders {

						nstrm.SetState(StreamStateClosed)
						closeStream(strm)

						if sc.debug {
							sc.logger.Printf("Cancelling stream in idle state: %d\n", nstrm.ID())
						}

						sc.writeReset(nstrm.ID(), StreamCanceled)

						continue
					}

					break
				}

				if sc.maxIdleTimer != nil {
					sc.resetMaxIdleTimer()
				}
			}

			if fr.Type() == FrameData {
				payloadLen := fr.Len()
				if err := sc.consumeConnectionWindow(payloadLen); err != nil {
					sc.writeError(nil, err)
					if isConnectionError(err) {
						sc.signalConnError()
					}
					if strm != nil {
						strm.SetState(StreamStateClosed)
					}
					break loop
				}
			}

			if err := sc.handleFrame(strm, fr); err != nil {
				sc.writeError(strm, err)
				strm.SetState(StreamStateClosed)
				if isConnectionError(err) {
					sc.signalConnError()
					break loop
				}
			}

			handleState(fr, strm)

			switch strm.State() {
			case StreamStateHalfClosed:
				if !strm.responseStarted {
					strm.responseStarted = true
					sc.handleEndRequest(strm)
				}

				if sc.responseComplete(strm) {
					strm.SetState(StreamStateClosed)
				}
			}

			// Fallback: if the client signaled EndStream on the request but the
			// state machine didn't land in HalfClosed (for example due to some
			// unexpected sequencing), ensure the response still starts once.
			if !strm.responseStarted && fr.Type() == FrameHeaders && fr.Flags().Has(FlagEndStream) {
				strm.responseStarted = true
				sc.handleEndRequest(strm)
			}

			if strm.State() == StreamStateClosed {
				closeStream(strm)
			}

			if isClosing {
				ref := atomic.LoadUint32(&sc.closeRef)
				// if there's no reference, then just close the connection
				if ref == 0 {
					break
				}

				// if we have a ref, then check that all streams previous to that ref are closed
				for _, strm := range strms {
					// if the stream is here, then it's not closed yet
					if strm.origType == FrameHeaders && strm.ID() <= ref {
						continue loop
					}
				}

				break loop
			}
		}
	}
}

func (sc *serverConn) writeReset(strm uint32, code ErrorCode) {
	r := AcquireFrame(FrameResetStream).(*RstStream)

	fr := AcquireFrameHeader()
	fr.SetStream(strm)
	fr.SetBody(r)

	r.SetCode(code)

	sc.enqueueFrame(fr)

	if sc.debug {
		sc.logger.Printf(
			"%s: Reset(stream=%d, code=%s)\n",
			sc.c.RemoteAddr(), strm, code,
		)
	}
}

func (sc *serverConn) writeGoAway(strm uint32, code ErrorCode, message string) {
	ga := AcquireFrame(FrameGoAway).(*GoAway)

	fr := AcquireFrameHeader()

	ga.SetStream(strm)
	ga.SetCode(code)
	ga.SetData([]byte(message))

	fr.SetBody(ga)

	sc.enqueueFrame(fr)

	if strm != 0 {
		atomic.StoreUint32(&sc.closeRef, sc.lastID)
	}

	atomic.StoreInt32((*int32)(&sc.state), int32(connStateClosed))

	if sc.debug {
		sc.logger.Printf(
			"%s: GoAway(stream=%d, code=%s): %s\n",
			sc.c.RemoteAddr(), strm, code, message,
		)
	}
}

func (sc *serverConn) writeGoAwayWithTimeout(strm uint32, code ErrorCode, message string, timeout time.Duration) bool {
	ga := AcquireFrame(FrameGoAway).(*GoAway)

	fr := AcquireFrameHeader()

	ga.SetStream(strm)
	ga.SetCode(code)
	ga.SetData([]byte(message))

	fr.SetBody(ga)

	sent := sc.enqueueFrameWithTimeout(fr, timeout)
	if sent && strm != 0 {
		atomic.StoreUint32(&sc.closeRef, sc.lastID)
	}

	atomic.StoreInt32((*int32)(&sc.state), int32(connStateClosed))

	return sent
}

func (sc *serverConn) enqueueFrameWithTimeout(fr *FrameHeader, d time.Duration) bool {
	sent := sc.enqueueFrameInternal(fr, d)
	if !sent {
		ReleaseFrameHeader(fr)
	}

	return sent
}

func (sc *serverConn) writeError(strm *Stream, err error) {
	streamErr := Error{}
	if !errors.As(err, &streamErr) {
		sc.writeReset(strm.ID(), InternalError)
		strm.SetState(StreamStateClosed)
		return
	}

	switch streamErr.frameType {
	case FrameGoAway:
		if strm == nil {
			sc.writeGoAway(0, streamErr.Code(), streamErr.Error())
		} else {
			sc.writeGoAway(strm.ID(), streamErr.Code(), streamErr.Error())
		}
	case FrameResetStream:
		sc.writeReset(strm.ID(), streamErr.Code())
	}

	if strm != nil {
		strm.SetState(StreamStateClosed)
	}
}

func isConnectionError(err error) bool {
	var h2Err Error
	if errors.As(err, &h2Err) {
		return h2Err.frameType == FrameGoAway
	}

	return false
}

func handleState(fr *FrameHeader, strm *Stream) {
	if fr.Type() == FrameResetStream {
		strm.SetState(StreamStateClosed)
	}

	switch strm.State() {
	case StreamStateIdle:
		if fr.Type() == FrameHeaders {
			strm.SetState(StreamStateOpen)
			if fr.Flags().Has(FlagEndStream) {
				if strm.headersFinished {
					strm.SetState(StreamStateHalfClosed)
				} else {
					strm.pendingEndStream = true
				}
			}
		} // TODO: else push promise ...
	case StreamStateReserved:
		// TODO: ...
	case StreamStateOpen:
		if fr.Flags().Has(FlagEndStream) {
			if strm.headersFinished {
				strm.SetState(StreamStateHalfClosed)
			} else {
				strm.pendingEndStream = true
			}
		} else if fr.Type() == FrameResetStream {
			strm.SetState(StreamStateClosed)
		}
	case StreamStateHalfClosed:
		// a stream can only go from HalfClosed to Closed if the client
		// sends a ResetStream frame.
		if fr.Type() == FrameResetStream {
			strm.SetState(StreamStateClosed)
		}
	case StreamStateClosed:
	}
}

var logger = log.New(os.Stdout, "[HTTP/2] ", log.LstdFlags)

var ctxPool = sync.Pool{
	New: func() any {
		return &fasthttp.RequestCtx{}
	},
}

func (sc *serverConn) createStream(c net.Conn, frameType FrameType, strm *Stream) {
	ctx := ctxPool.Get().(*fasthttp.RequestCtx)
	ctx.Request.Reset()
	ctx.Response.Reset()

	ctx.Init2(c, sc.logger, false)

	strm.origType = frameType
	strm.startedAt = time.Now()
	strm.SetData(ctx)
}

func (sc *serverConn) handleFrame(strm *Stream, fr *FrameHeader) error {
	err := sc.verifyState(strm, fr)
	if err != nil {
		return err
	}

	switch fr.Type() {
	case FrameHeaders, FrameContinuation:
		if strm.State() >= StreamStateHalfClosed {
			return NewGoAwayError(ProtocolError, "received headers on a finished stream")
		}

		err = sc.handleHeaderFrame(strm, fr)
		if err != nil {
			return err
		}

		if fr.Flags().Has(FlagEndHeaders) {
			// headers are only finished if there's no previousHeaderBytes
			strm.headersFinished = len(strm.previousHeaderBytes) == 0
			if !strm.headersFinished {
				return NewGoAwayError(CompressionError, "END_HEADERS received on an incomplete stream")
			}

			if strm.headersFinished && strm.pendingEndStream {
				strm.SetState(StreamStateHalfClosed)
				strm.pendingEndStream = false
			}

			// calling req.URI() triggers a URL parsing, so because of that we need to delay the URL parsing.
			strm.ctx.Request.URI().SetSchemeBytes(strm.scheme)
		}
	case FrameData:
		if !strm.headersFinished {
			return NewGoAwayError(ProtocolError, "stream didn't end the headers")
		}

		if strm.State() >= StreamStateHalfClosed {
			return NewGoAwayError(StreamClosedError, "stream closed")
		}

		data := fr.Body().(*Data)
		payloadLen := fr.Len()

		if err := sc.consumeStreamWindow(strm, payloadLen); err != nil {
			return err
		}

		strm.ctx.Request.AppendBody(data.Data())
		sc.queueWindowUpdate(strm, payloadLen)
	case FrameResetStream:
		if strm.State() == StreamStateIdle {
			return NewGoAwayError(ProtocolError, "RST_STREAM on idle stream")
		}
	case FramePriority:
		if strm.State() != StreamStateIdle && !strm.headersFinished {
			return NewGoAwayError(ProtocolError, "frame priority on an open stream")
		}

		if priorityFrame, ok := fr.Body().(*Priority); ok && priorityFrame.Stream() == strm.ID() {
			return NewGoAwayError(ProtocolError, "stream that depends on itself")
		}
	case FrameWindowUpdate:
		if strm.State() == StreamStateIdle {
			return NewGoAwayError(ProtocolError, "window update on idle stream")
		}

		win := int64(fr.Body().(*WindowUpdate).Increment())
		if incErr := validateWindowIncrement(win); incErr != nil {
			code := FlowControlError
			if errors.Is(incErr, errWindowIncrementZero) {
				code = ProtocolError
			}
			return NewResetStreamError(code, windowUpdateErrorMessage(incErr))
		}

		if _, winErr := addAndClampWindow(&strm.sendWindow, win); winErr != nil {
			return NewResetStreamError(FlowControlError, windowUpdateErrorMessage(winErr))
		}

		sc.flushPendingData(strm)
	default:
		return NewGoAwayError(ProtocolError, "invalid frame")
	}

	return err
}

func (sc *serverConn) handleHeaderFrame(strm *Stream, fr *FrameHeader) error {
	if strm.headersFinished && !fr.Flags().Has(FlagEndStream|FlagEndHeaders) {
		// TODO handle trailers
		return NewGoAwayError(ProtocolError, "stream not open")
	}

	if headerFrame, ok := fr.Body().(*Headers); ok && headerFrame.Stream() == strm.ID() {
		return NewGoAwayError(ProtocolError, "stream that depends on itself")
	}

	b := append(strm.previousHeaderBytes, fr.Body().(FrameWithHeaders).Headers()...)
	hf := AcquireHeaderField()
	req := &strm.ctx.Request

	var err error

	strm.previousHeaderBytes = strm.previousHeaderBytes[:0]
	fieldsProcessed := 0

	for len(b) > 0 {
		pb := b

		b, err = sc.dec.nextField(hf, strm.headerBlockNum, fieldsProcessed, b)
		if err != nil {
			if errors.Is(err, ErrUnexpectedSize) && len(pb) > 0 {
				err = nil
				strm.previousHeaderBytes = append(strm.previousHeaderBytes, pb...)
			} else {
				err = NewGoAwayError(CompressionError, err.Error())
			}

			break
		}

		k, v := hf.KeyBytes(), hf.ValueBytes()
		if hf.IsPseudo() {
			if strm.regularHeaderSeen {
				return NewResetStreamError(ProtocolError, "pseudo-header after regular header")
			}

			switch {
			case bytes.Equal(k, StringMethod):
				if strm.seenMethod {
					return NewResetStreamError(ProtocolError, "duplicate pseudo-header :method")
				}

				strm.seenMethod = true
				strm.isConnect = bytes.Equal(v, StringCONNECT)
				if strm.isConnect && (strm.seenPath || strm.seenScheme) {
					return NewResetStreamError(ProtocolError, "CONNECT must not include :path or :scheme")
				}
				req.Header.SetMethodBytes(v)
			case bytes.Equal(k, StringPath):
				if strm.seenPath {
					return NewResetStreamError(ProtocolError, "duplicate pseudo-header :path")
				}

				if strm.isConnect {
					return NewResetStreamError(ProtocolError, "CONNECT must not include :path or :scheme")
				}

				strm.seenPath = true
				req.Header.SetRequestURIBytes(v)
			case bytes.Equal(k, StringScheme):
				if strm.seenScheme {
					return NewResetStreamError(ProtocolError, "duplicate pseudo-header :scheme")
				}

				if strm.isConnect {
					return NewResetStreamError(ProtocolError, "CONNECT must not include :path or :scheme")
				}

				strm.seenScheme = true
				strm.scheme = append(strm.scheme[:0], v...)
			case bytes.Equal(k, StringAuthority):
				if strm.seenAuthority {
					return NewResetStreamError(ProtocolError, "duplicate pseudo-header :authority")
				}

				strm.seenAuthority = true
				req.Header.SetHostBytes(v)
				req.Header.AddBytesV("Host", v)
			default:
				return NewResetStreamError(ProtocolError, fmt.Sprintf("unknown pseudo-header %s", k))
			}
		} else {
			if forbidden, name := isForbiddenHeader(k, v); forbidden {
				return NewGoAwayError(ProtocolError, fmt.Sprintf("forbidden header field %s", name))
			}

			strm.regularHeaderSeen = true

			switch {
			case bytes.Equal(k, StringUserAgent):
				req.Header.SetUserAgentBytes(v)
			case bytes.Equal(k, StringContentType):
				req.Header.SetContentTypeBytes(v)
			default:
				req.Header.AddBytesKV(k, v)
			}
		}

		fieldsProcessed++
	}

	strm.headerBlockNum++

	if err == nil && fr.Flags().Has(FlagEndHeaders) && len(strm.previousHeaderBytes) == 0 {
		if !strm.seenMethod {
			return NewResetStreamError(ProtocolError, "missing required pseudo-header")
		}

		if strm.isConnect {
			if !strm.seenAuthority {
				return NewResetStreamError(ProtocolError, "missing required pseudo-header")
			}

			if strm.seenPath || strm.seenScheme {
				return NewResetStreamError(ProtocolError, "CONNECT must not include :path or :scheme")
			}
		} else if !strm.seenScheme || !strm.seenPath {
			return NewResetStreamError(ProtocolError, "missing required pseudo-header")
		}
	}

	return err
}

func isForbiddenHeader(key, value []byte) (bool, string) {
	switch {
	case bytes.EqualFold(key, []byte("connection")):
		return true, "connection"
	case bytes.EqualFold(key, []byte("proxy-connection")):
		return true, "proxy-connection"
	case bytes.EqualFold(key, []byte("transfer-encoding")):
		return true, "transfer-encoding"
	case bytes.EqualFold(key, []byte("upgrade")):
		return true, "upgrade"
	case bytes.EqualFold(key, []byte("keep-alive")):
		return true, "keep-alive"
	case bytes.EqualFold(key, []byte("te")):
		if !bytes.EqualFold(bytes.TrimSpace(value), []byte("trailers")) {
			return true, "te"
		}
	}

	return false, ""
}

func (sc *serverConn) verifyState(strm *Stream, fr *FrameHeader) error {
	switch strm.State() {
	case StreamStateIdle:
		if fr.Type() != FrameHeaders && fr.Type() != FramePriority {
			return NewGoAwayError(ProtocolError, "wrong frame on idle stream")
		}
	case StreamStateHalfClosed:
		if fr.Type() != FrameWindowUpdate && fr.Type() != FramePriority && fr.Type() != FrameResetStream {
			return NewGoAwayError(StreamClosedError, "wrong frame on half-closed stream")
		}
	default:
	}

	return nil
}

// handleEndRequest dispatches the finished request to the handler.
func (sc *serverConn) handleEndRequest(strm *Stream) {
	ctx := strm.ctx
	ctx.Request.Header.SetProtocolBytes(StringHTTP2)

	sc.h(ctx)

	hasBody := ctx.Response.IsBodyStream() || len(ctx.Response.Body()) > 0

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetEndHeaders(true)
	h.SetEndStream(!hasBody)

	fr.SetBody(h)

	sc.encMu.Lock()
	fasthttpResponseHeaders(h, &sc.enc, &ctx.Response)
	sc.encMu.Unlock()

	sc.enqueueFrame(fr)
	if !hasBody {
		sc.markResponseEnded(strm)
	}

	if hasBody {
		if ctx.Response.IsBodyStream() {
			streamWriter := acquireStreamWrite()
			streamWriter.strm = strm
			streamWriter.sc = sc
			streamWriter.writer = sc.writer
			streamWriter.size = int64(ctx.Response.Header.ContentLength())
			_ = ctx.Response.BodyWriteTo(streamWriter)
			releaseStreamWrite(streamWriter)
		} else {
			sc.queueData(strm, ctx.Response.Body(), true)
		}
	}
}

var (
	copyBufPool = sync.Pool{
		New: func() any {
			return make([]byte, 1<<14) // max frame size 16384
		},
	}
	streamWritePool = sync.Pool{
		New: func() any {
			return &streamWrite{}
		},
	}
)

type streamWrite struct {
	size    int64
	written int64
	strm    *Stream
	sc      *serverConn
	writer  chan<- *FrameHeader
}

func acquireStreamWrite() *streamWrite {
	v := streamWritePool.Get()
	if v == nil {
		return &streamWrite{}
	}
	return v.(*streamWrite)
}

func releaseStreamWrite(streamWrite *streamWrite) {
	streamWrite.Reset()
	streamWritePool.Put(streamWrite)
}

func (s *streamWrite) Reset() {
	s.size = 0
	s.written = 0
	s.strm = nil
	s.sc = nil
	s.writer = nil
}

func (s *streamWrite) sendFrame(fr *FrameHeader) {
	if s.writer == nil {
		ReleaseFrameHeader(fr)
		return
	}

	defer func() {
		if r := recover(); r != nil {
			ReleaseFrameHeader(fr)
		}
	}()

	s.writer <- fr
}

func (s *streamWrite) Write(body []byte) (n int, err error) {
	if (s.size <= 0 && s.written > 0) || (s.size > 0 && s.written >= s.size) {
		return 0, errors.New("writer closed")
	}

	if s.sc == nil {
		step := 1 << 14 // max frame size 16384
		n = len(body)
		s.written += int64(n)

		end := s.size < 0 || s.written >= s.size
		for i := 0; i < n; i += step {
			if i+step >= n {
				step = n - i
			}

			fr := AcquireFrameHeader()
			fr.SetStream(s.strm.ID())

			data := AcquireFrame(FrameData).(*Data)
			data.SetEndStream(end && i+step == n)
			data.SetPadding(false)
			data.SetData(body[i : step+i])

			fr.SetBody(data)

			s.sendFrame(fr)
		}

		return len(body), nil
	}

	n = len(body)
	s.written += int64(n)

	end := s.size < 0 || s.written >= s.size
	s.sc.queueData(s.strm, body, end)

	return len(body), nil
}

func (s *streamWrite) ReadFrom(r io.Reader) (num int64, err error) {
	buf := copyBufPool.Get().([]byte)

	if s.size < 0 {
		lrSize := limitedReaderSize(r)
		if lrSize >= 0 {
			s.size = lrSize
		}
	}

	var n int
	for {
		n, err = r.Read(buf[0:])
		if n <= 0 && err == nil {
			err = errors.New("BUG: io.Reader returned 0, nil")
		}

		if err != nil {
			break
		}

		if s.sc == nil {
			fr := AcquireFrameHeader()
			fr.SetStream(s.strm.ID())

			data := AcquireFrame(FrameData).(*Data)
			data.SetEndStream(err != nil || (s.size >= 0 && num+int64(n) >= s.size))
			data.SetPadding(false)
			data.SetData(buf[:n])
			fr.SetBody(data)

			s.writer <- fr
		} else {
			s.sc.queueData(
				s.strm,
				buf[:n],
				err != nil || (s.size >= 0 && num+int64(n) >= s.size),
			)
		}

		num += int64(n)
		if s.size >= 0 && num >= s.size {
			break
		}
	}

	copyBufPool.Put(buf)
	if errors.Is(err, io.EOF) {
		return num, nil
	}

	return num, err
}

func (sc *serverConn) sendPingAndSchedule() {
	sc.pingTimerMu.Lock()
	if sc.pingTimer == nil || sc.isClosed() {
		sc.pingTimerMu.Unlock()
		return
	}
	sc.pingTimerMu.Unlock()

	sc.writePing()

	sc.pingTimerMu.Lock()
	if sc.pingTimer != nil && !sc.isClosed() {
		sc.pingTimer.Reset(sc.pingInterval)
	}
	sc.pingTimerMu.Unlock()
}

func (sc *serverConn) writeLoop() {
	defer sc.stopPingTimer()

	if sc.pingInterval > 0 {
		sc.pingTimerMu.Lock()
		if !sc.isClosed() {
			sc.pingTimer = time.AfterFunc(sc.pingInterval, sc.sendPingAndSchedule)
		}
		sc.pingTimerMu.Unlock()
	}

	buffered := 0

	for fr := range sc.writer {
		if fr.Type() == FrameData {
			if strm := sc.getActiveStream(fr.Stream()); strm != nil {
				if data, ok := fr.Body().(*Data); ok {
					frameLen := fr.Len()
					if frameLen == 0 {
						frameLen = len(data.Data())
					}

					if atomic.LoadInt64(&strm.sendWindow) < 0 {
						atomic.AddInt64(&strm.sendWindow, int64(frameLen))
						atomic.AddInt64(&sc.clientWindow, int64(frameLen))

						sc.queueData(strm, append([]byte(nil), data.Data()...), data.EndStream())
						ReleaseFrameHeader(fr)
						continue
					}
				}
			}
		}

		_, err := fr.WriteTo(sc.bw)
		if err == nil && (len(sc.writer) == 0 || buffered > 10) {
			err = sc.bw.Flush()
			buffered = 0
		} else if err == nil {
			buffered++
		}

		ReleaseFrameHeader(fr)

		if err != nil {
			sc.logger.Printf("ERROR: writeLoop: %s\n", err)
			sc.handleWriteLoopFailure()
			sc.drainWriter()
			return
		}
	}
}

func (sc *serverConn) handleWriteLoopFailure() {
	sc.signalConnClose()
	sc.closeCloser()
	sc.close()
	sc.closeWriter()
}

func (sc *serverConn) drainWriter() {
	if sc.writer == nil {
		return
	}
	for fr := range sc.writer {
		ReleaseFrameHeader(fr)
	}
}

func (sc *serverConn) handleSettings(st *Settings) {
	oldInitWin := atomic.LoadInt64(&sc.initialClientWindow)

	sc.clientSMu.Lock()
	st.CopyTo(&sc.clientS)

	newInitWin := oldInitWin
	if st.HasMaxWindowSize() {
		newInitWin = int64(st.MaxWindowSize())
	} else if newInitWin == 0 {
		// Initialize to default if we haven't applied any client window yet
		newInitWin = int64(defaultWindowSize)
	}

	maxFrameSize := sc.clientS.MaxFrameSize()
	if maxFrameSize == 0 {
		maxFrameSize = defaultDataFrameSize
	}

	headerTableSize := sc.clientS.HeaderTableSize()
	sc.clientSMu.Unlock()

	sc.encMu.Lock()
	sc.enc.SetMaxTableSize(headerTableSize)
	sc.encMu.Unlock()

	atomic.StoreInt64(&sc.clientMaxFrameSize, int64(maxFrameSize))

	delta := newInitWin - oldInitWin

	// Hold streamsMu while updating initialClientWindow and adjusting streams
	// to prevent race with stream creation and queueData
	sc.streamsMu.Lock()
	streamIDsToFlush := make([]uint32, 0, len(sc.streams))
	if delta != 0 {
		for _, strm := range sc.streams {
			newWin := atomic.AddInt64(&strm.sendWindow, delta)
			if newWin > maxWindowSize {
				atomic.StoreInt64(&strm.sendWindow, maxWindowSize)
			}
			streamIDsToFlush = append(streamIDsToFlush, strm.ID())
		}
	} else {
		for _, strm := range sc.streams {
			streamIDsToFlush = append(streamIDsToFlush, strm.ID())
		}
	}
	atomic.StoreInt64(&sc.initialClientWindow, newInitWin)

	// Send ACK before releasing the stream lock so no DATA is flushed ahead of
	// the acknowledgement when the window opens.
	fr := AcquireFrameHeader()

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	sc.enqueueFrame(fr)
	sc.streamsMu.Unlock()

	// Flush pending data after sending the ACK so the client observes the
	// acknowledgement before any DATA that becomes sendable due to the window
	// update. This avoids h2spec consuming the DATA while waiting for the ACK.
	for _, id := range streamIDsToFlush {
		if strm := sc.getActiveStream(id); strm != nil {
			sc.flushPendingData(strm)
		}
	}

	// If the initial window just increased, schedule a follow-up flush to
	// catch streams that might queue data immediately after this settings frame
	// (e.g. when HEADERS and SETTINGS arrive back-to-back).
	if st.HasMaxWindowSize() && delta > 0 {
		// Do a few retries with small delays to cover races between incoming
		// SETTINGS and the request handler queueing its response.
		go func() {
			for i := 0; i < 3; i++ {
				time.Sleep(10 * time.Millisecond)
				sc.forEachActiveStream(func(strm *Stream) {
					sc.flushPendingData(strm)
				})
			}
			time.Sleep(50 * time.Millisecond)
			sc.forEachActiveStream(func(strm *Stream) {
				sc.flushPendingData(strm)
			})
		}()
	}
}

const maxWindowIncrement = 1<<31 - 1
const maxWindowSize = maxWindowIncrement

var (
	errInvalidWindowSizeIncrement = errors.New("invalid window size increment")
	errWindowSizeOverflow         = errors.New("window size overflow")
	errWindowIncrementZero        = errors.New("window size increment is 0")
)

func validateWindowIncrement(inc int64) error {
	if inc == 0 {
		return errWindowIncrementZero
	}

	if inc < 0 || inc > maxWindowSize {
		return errInvalidWindowSizeIncrement
	}

	return nil
}

func (sc *serverConn) addActiveStream(strm *Stream) {
	sc.streamsMu.Lock()
	if sc.streams == nil {
		sc.streams = make(map[uint32]*Stream)
	}
	sc.streams[strm.ID()] = strm
	sc.streamsMu.Unlock()
}

func (sc *serverConn) deleteActiveStream(id uint32) {
	sc.streamsMu.Lock()
	delete(sc.streams, id)
	sc.streamsMu.Unlock()
}

func (sc *serverConn) forEachActiveStream(fn func(*Stream)) {
	sc.streamsMu.Lock()
	streams := make([]*Stream, 0, len(sc.streams))
	for _, strm := range sc.streams {
		streams = append(streams, strm)
	}
	sc.streamsMu.Unlock()

	for _, strm := range streams {
		fn(strm)
	}
}

func (sc *serverConn) getActiveStream(id uint32) *Stream {
	sc.streamsMu.Lock()
	strm := sc.streams[id]
	sc.streamsMu.Unlock()
	return strm
}

func addAndClampWindow(window *int64, inc int64) (int64, error) {
	if inc <= 0 || inc > maxWindowSize {
		return atomic.LoadInt64(window), errInvalidWindowSizeIncrement
	}

	for {
		current := atomic.LoadInt64(window)
		if current >= maxWindowSize {
			return maxWindowSize, nil
		}

		if current > maxWindowSize-inc {
			atomic.StoreInt64(window, maxWindowSize)
			return maxWindowSize, errWindowSizeOverflow
		}

		next := current + inc
		if atomic.CompareAndSwapInt64(window, current, next) {
			return next, nil
		}
	}
}

func windowUpdateErrorMessage(err error) string {
	switch err {
	case errInvalidWindowSizeIncrement:
		return "invalid window size increment"
	case errWindowSizeOverflow:
		return "window is above limits"
	case errWindowIncrementZero:
		return "window size increment is 0"
	default:
		return err.Error()
	}
}

func (sc *serverConn) queueDataFrame(strm *Stream, data []byte, endStream bool) {
	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())

	payload := AcquireFrame(FrameData).(*Data)
	payload.SetEndStream(endStream)
	payload.SetPadding(false)
	payload.SetData(data)

	fr.SetBody(payload)

	sc.enqueueFrame(fr)
}

func (sc *serverConn) appendPendingData(strm *Stream, data []byte, endStream bool) {
	strm.pendingMu.Lock()
	strm.pendingData = append(strm.pendingData, data...)
	if endStream {
		strm.pendingDataEndStream = true
	}
	strm.pendingMu.Unlock()
}

func (sc *serverConn) markResponseEnded(strm *Stream) {
	strm.pendingMu.Lock()
	strm.responseEnded = true
	strm.pendingMu.Unlock()
}

func (sc *serverConn) responseComplete(strm *Stream) bool {
	strm.pendingMu.Lock()
	defer strm.pendingMu.Unlock()
	return strm.responseEnded && len(strm.pendingData) == 0 && !strm.pendingDataEndStream
}

func (sc *serverConn) queueData(strm *Stream, data []byte, endStream bool) {
	if len(data) == 0 {
		if endStream {
			sc.markResponseEnded(strm)
			sc.queueDataFrame(strm, nil, true)
		}
		return
	}

	maxFrame := int(atomic.LoadInt64(&sc.clientMaxFrameSize))
	if maxFrame <= 0 {
		maxFrame = 1 << 14
	}

	// Note: initialClientWindow and clientWindow are properly initialized in Serve()
	// and updated via handleSettings. Window value of 0 is valid (means flow control
	// is blocking all data), so we should not override it.

	for len(data) > 0 {
		// Hold streamsMu to ensure atomic operation with handleSettings window adjustments
		sc.streamsMu.Lock()

		streamWin := atomic.LoadInt64(&strm.sendWindow)
		connWin := atomic.LoadInt64(&sc.clientWindow)

		if streamWin <= 0 || connWin <= 0 {
			sc.appendPendingData(strm, data, endStream)
			sc.streamsMu.Unlock()
			return
		}

		toSend := min(len(data), maxFrame, int(streamWin), int(connWin))
		if toSend <= 0 {
			sc.streamsMu.Unlock()
			sc.appendPendingData(strm, data, endStream)
			return
		}

		atomic.AddInt64(&strm.sendWindow, -int64(toSend))
		atomic.AddInt64(&sc.clientWindow, -int64(toSend))

		chunk := data[:toSend]
		data = data[toSend:]
		isLastChunk := endStream && len(data) == 0
		if isLastChunk {
			sc.markResponseEnded(strm)
		}
		sc.queueDataFrame(strm, chunk, isLastChunk)

		sc.streamsMu.Unlock()
	}
}

func (sc *serverConn) flushPendingData(strm *Stream) {
	strm.pendingMu.Lock()
	hasPending := len(strm.pendingData) > 0 || strm.pendingDataEndStream
	data := append([]byte(nil), strm.pendingData...)
	endStream := strm.pendingDataEndStream
	strm.pendingData = strm.pendingData[:0]
	strm.pendingDataEndStream = false
	strm.pendingMu.Unlock()

	if !hasPending {
		return
	}

	sc.queueData(strm, data, endStream)
}

func (sc *serverConn) consumeConnectionWindow(n int) error {
	if n == 0 {
		return nil
	}

	if n < 0 {
		return NewGoAwayError(FlowControlError, "invalid DATA size")
	}

	if int32(n) > sc.currentWindow {
		return NewGoAwayError(FlowControlError, "connection flow control window exceeded")
	}

	sc.currentWindow -= int32(n)
	return nil
}

func (sc *serverConn) consumeStreamWindow(strm *Stream, n int) error {
	if n == 0 {
		return nil
	}

	if strm == nil || n < 0 {
		return NewResetStreamError(FlowControlError, "invalid DATA size")
	}

	if int64(n) > atomic.LoadInt64(&strm.window) {
		return NewResetStreamError(FlowControlError, "stream flow control window exceeded")
	}

	atomic.AddInt64(&strm.window, -int64(n))
	return nil
}

func (sc *serverConn) queueWindowUpdate(strm *Stream, n int) {
	if n <= 0 || strm == nil {
		return
	}

	connInc := sc.windowIncrement(int64(sc.maxWindow), int64(sc.currentWindow), n)
	streamInc := sc.windowIncrement(int64(strm.recvWindowSize), atomic.LoadInt64(&strm.window), n)

	if connInc > 0 {
		sc.sendWindowIncrement(0, connInc, func(inc int) {
			sc.currentWindow += int32(inc)
		})
	}

	if streamInc > 0 {
		sc.sendWindowIncrement(strm.ID(), streamInc, func(inc int) {
			atomic.AddInt64(&strm.window, int64(inc))
		})
	}
}

func (sc *serverConn) windowIncrement(limit, current int64, n int) int {
	if n <= 0 || current >= limit {
		return 0
	}

	remaining := limit - current
	if remaining <= 0 {
		return 0
	}

	if int64(n) > remaining {
		return int(remaining)
	}

	return n
}

func (sc *serverConn) sendWindowIncrement(streamID uint32, n int, apply func(int)) {
	for remaining := n; remaining > 0; {
		inc := min(remaining, maxWindowIncrement)

		apply(inc)

		fr := AcquireFrameHeader()
		wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
		wu.SetIncrement(inc)

		fr.SetStream(streamID)
		fr.SetBody(wu)

		sc.enqueueFrame(fr)

		remaining -= inc
	}
}

func fasthttpResponseHeaders(dst *Headers, hp *HPACK, res *fasthttp.Response) {
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	hf.SetKeyBytes(StringStatus)
	hf.SetValue(
		strconv.FormatInt(
			int64(res.Header.StatusCode()), 10,
		),
	)

	dst.AppendHeaderField(hp, hf, true)

	if !res.IsBodyStream() {
		res.Header.SetContentLength(len(res.Body()))
	}
	// Remove the Connection field
	res.Header.Del("Connection")
	// Remove the Transfer-Encoding field
	res.Header.Del("Transfer-Encoding")

	res.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(ToLower(k), v)
		dst.AppendHeaderField(hp, hf, false)
	})
}

func limitedReaderSize(r io.Reader) int64 {
	lr, ok := r.(*io.LimitedReader)
	if !ok {
		return -1
	}
	return lr.N
}
