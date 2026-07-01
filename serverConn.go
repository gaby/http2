package http2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"runtime/debug"
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

// validHeaderNameChar is a 256-byte lookup table where a non-zero value
// indicates that the byte is valid inside an HTTP/2 header field name
// (excluding the leading ':' for pseudo-headers, handled separately).
var validHeaderNameChar [256]bool

func init() {
	for ch := byte('a'); ch <= 'z'; ch++ {
		validHeaderNameChar[ch] = true
	}
	for ch := byte('0'); ch <= '9'; ch++ {
		validHeaderNameChar[ch] = true
	}
	for _, ch := range []byte("!#$%&'*+-.^_`|~") {
		validHeaderNameChar[ch] = true
	}
}

type serverConn struct {
	pingWindowStart time.Time
	rstWindowStart  time.Time
	c               net.Conn
	logger          fasthttp.Logger

	h fasthttp.RequestHandler

	br *bufio.Reader
	bw *bufio.Writer

	writer  chan *FrameHeader
	reader  chan *FrameHeader
	streams map[uint32]*Stream

	// pingTimer
	pingTimer       *time.Timer
	maxRequestTimer *time.Timer
	maxIdleTimer    *time.Timer

	closer chan struct{}

	enc HPACK
	dec HPACK

	st      Settings
	clientS Settings

	// client's window
	// should be int64 because the user can try to overflow it
	clientWindow int64
	// initial window advertised by the client for new streams
	initialClientWindow atomic.Int64
	// client's max frame size
	clientMaxFrameSize atomic.Int64

	// maxRequestTime is the max time of a request over one single stream
	maxRequestTime time.Duration
	pingInterval   time.Duration
	// maxIdleTime is the max time a client can be connected without sending any REQUEST.
	// As highlighted, PING/PONG frames are completely excluded.
	//
	// Therefore, a client that didn't send a request for more than `maxIdleTime` will see it's connection closed.
	maxIdleTime time.Duration

	// enqueueTimeout overrides defaultEnqueueTimeout when non-zero.
	enqueueTimeout time.Duration

	pingCount      int
	pingViolations int

	rstCount int
	writerMu sync.RWMutex

	closerOnce      sync.Once
	writerCloseOnce sync.Once

	encMu sync.Mutex // guards enc
	// clientSMu guards access to clientS
	clientSMu sync.Mutex

	streamsMu   sync.Mutex
	pingTimerMu sync.Mutex
	timerMu     sync.Mutex

	// PING rate-limiting state (guards with pingMu).
	pingMu sync.Mutex

	// RST_STREAM flood-detection state (guards with rstMu).
	rstMu sync.Mutex

	// last valid ID used as a reference for new IDs
	lastID uint32

	// our values
	maxWindow     int32
	currentWindow int32
	nextPushID    atomic.Uint32 // next even stream ID for server push
	writerClosed  atomic.Bool

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
	closeRef atomic.Uint32

	debug bool
}

// defaultEnqueueTimeout bounds how long a sender will wait when the writer
// channel is full. A generous timeout avoids dropping frames under normal
// load, while still letting shutdown make progress if the writer loop is
// wedged.
const defaultEnqueueTimeout = 2 * time.Second

// goawayFlushDelay is a brief pause given to the write loop to flush a GOAWAY
// frame to the peer before the connection is torn down. The value must be large
// enough for the writeLoop goroutine to be scheduled, flush the frame, AND for
// the OS TCP stack to deliver the data to the peer. On Windows CI runners the
// kernel may need 100ms+ for a context switch plus TCP delivery.
const goawayFlushDelay = 200 * time.Millisecond

// maxContinuationFrames is the maximum number of CONTINUATION frames allowed
// for a single header block. A client fragmenting headers across more frames
// than this is treated as a CONTINUATION flood (CVE-2024-27983).
const maxContinuationFrames = 64

// maxRawHeaderBlockSize is the maximum raw (compressed) size of a header block
// accumulated across HEADERS + CONTINUATION frames before HPACK decoding.
// 64 frames × 16 KiB per frame = 1 MiB.
const maxRawHeaderBlockSize = maxContinuationFrames * defaultDataFrameSize

// PING rate-limiting parameters.
const (
	pingRateMax      = 10          // maximum PINGs per window
	pingRateWindow   = time.Second // sliding window duration
	pingMaxViolation = 3           // consecutive windows over limit before GOAWAY
)

// RST flood-detection parameters.
const (
	rstRateMax    = 100         // maximum RST_STREAM frames per window
	rstRateWindow = time.Second // sliding window duration
)

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
			// Safety-net close: the normal shutdown path (writeLoop's defer)
			// closes the connection after flushing all queued frames. This
			// timer only fires when the writeLoop is stuck. Use a generous
			// delay so that GOAWAY frames have time to be written, flushed,
			// and delivered by the OS TCP stack even on slow CI platforms
			// (Windows runners may need 200ms+ for scheduling + TCP flush).
			time.AfterFunc(500*time.Millisecond, func() {
				_ = sc.c.Close()
			})
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
	timeout := sc.enqueueTimeout
	if timeout <= 0 {
		timeout = defaultEnqueueTimeout
	}
	if sc.enqueueFrameInternal(fr, timeout) {
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

	defer func() {
		if r := recover(); r != nil {
			if sc.debug {
				sc.logger.Printf("enqueueFrame: writer closed, dropping frame: %v\n", r)
			}
		}
	}()

	// Fast path: avoid timer allocation when writer has capacity.
	select {
	case sc.writer <- fr:
		return true
	default:
	}

	if d <= 0 {
		d = defaultEnqueueTimeout
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	if sc.closer == nil {
		select {
		case sc.writer <- fr:
			return true
		case <-timer.C:
			return false
		}
	}

	select {
	case sc.writer <- fr:
		return true
	case <-sc.closer:
		return false
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
	sc.initialClientWindow.Store(int64(initWin))
	sc.clientMaxFrameSize.Store(int64(defaultDataFrameSize))

	if sc.maxIdleTime > 0 {
		sc.maxIdleTimer = time.AfterFunc(sc.maxIdleTime, sc.closeIdleConn)
	}
	sc.timerMu.Unlock()

	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("Serve panicked (remote=%s): %s:\n%s\n",
				sc.c.RemoteAddr(), err, debug.Stack())
		}
	}()

	writerDone := make(chan struct{})
	go func() {
		defer func() {
			// Brief pause before closing so the OS TCP stack can flush any
			// remaining data (e.g. a GOAWAY frame) to the peer. On Windows,
			// Close() can discard unsent data in the kernel send buffer.
			time.Sleep(goawayFlushDelay)
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

	// Close the closer channel after all goroutines have finished to ensure
	// the handleSettings goroutine can exit cleanly.
	sc.closeCloser()

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

// isIdleReadTimeout reports whether err came from the idle-timeout read
// deadline backstop rather than from a shutdown already in progress.
// The shutdown flags are checked so connection teardown does not get mistaken
// for an idle timeout that still needs a graceful GOAWAY.
func (sc *serverConn) isIdleReadTimeout(err error) bool {
	var netErr net.Error
	isTimeout := errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout())
	return isTimeout &&
		sc.maxIdleTime > 0 &&
		!sc.closing.Load() &&
		!sc.connErr.Load()
}

func (sc *serverConn) handlePing(ping *Ping) {
	// Rate-limit inbound PINGs to protect against ping-flood DoS.
	// Violations are counted per completed window: a window that exceeded
	// pingRateMax increments the violation counter; a clean window resets it.
	// After pingMaxViolation consecutive over-limit windows the connection is
	// terminated with ENHANCE_YOUR_CALM.
	sc.pingMu.Lock()
	now := time.Now()
	windowExpired := sc.pingWindowStart.IsZero() || now.Sub(sc.pingWindowStart) >= pingRateWindow
	if windowExpired {
		// Settle the previous window's verdict before starting a new one.
		if !sc.pingWindowStart.IsZero() {
			if sc.pingCount > pingRateMax {
				sc.pingViolations++
			} else {
				sc.pingViolations = 0
			}
		}
		sc.pingWindowStart = now
		sc.pingCount = 0
	}
	sc.pingCount++
	overLimit := sc.pingCount > pingRateMax
	violations := sc.pingViolations
	sc.pingMu.Unlock()

	if violations >= pingMaxViolation {
		sc.writeGoAway(0, EnhanceYourCalm, "too many pings")
		time.AfterFunc(goawayFlushDelay, sc.signalConnError)
		return
	}

	if overLimit {
		// Silently drop the excess PING; the client is sending too fast.
		return
	}

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
			sc.logger.Printf("readLoop panicked (remote=%s): %s\n%s\n",
				sc.c.RemoteAddr(), err, debug.Stack())
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

			if sc.isIdleReadTimeout(err) {
				// The socket read deadline is an idle-timeout backstop. If it wins
				// the race against maxIdleTimer, still send GOAWAY so peers observe
				// a graceful idle shutdown instead of a raw transport error.
				sc.closeIdleConn()
				err = nil
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
				time.AfterFunc(goawayFlushDelay, sc.signalConnError)
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
			sc.streamsMu.Lock()
			streamCount := len(sc.streams)
			sc.streamsMu.Unlock()
			sc.logger.Printf("handleStreams panicked (remote=%s, openStreams=%d): %s\n%s\n",
				sc.c.RemoteAddr(), streamCount, err, debug.Stack())
		}
	}()

	var strms Streams
	var reqTimerArmed bool
	var openStreams int

	// maxTrackedClosedStreams bounds closedStrms. On a long-lived connection the
	// total number of streams served is unbounded and IDs strictly increase, so
	// entries far below lastID are never referenced again by a conformant peer.
	const maxTrackedClosedStreams = 1 << 12

	closedStrms := make(map[uint32]struct{})

	// markClosed records a closed/refused stream ID and keeps closedStrms bounded.
	markClosed := func(id uint32) {
		closedStrms[id] = struct{}{}
		if len(closedStrms) > maxTrackedClosedStreams {
			// ponytail: O(n) prune runs only when over the cap (amortized O(1) per
			// close); a frame on a pruned old ID is still rejected by the id<lastID guard.
			cutoff := uint32(0)
			if sc.lastID > maxTrackedClosedStreams {
				cutoff = sc.lastID - maxTrackedClosedStreams
			}
			for cid := range closedStrms {
				if cid < cutoff {
					delete(closedStrms, cid)
				}
			}
		}
	}

	markClosedStream := func(id uint32, origType FrameType) {
		markClosed(id)

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

		markClosed(strmID)
		strms.Del(strmID)

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

					ReleaseFrameHeader(fr)
					continue
				}

				if _, ok := closedStrms[fr.Stream()]; ok {
					if fr.Type() != FramePriority {
						sc.writeGoAway(fr.Stream(), StreamClosedError, "frame on closed stream")
					}

					ReleaseFrameHeader(fr)
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

					ReleaseFrameHeader(fr)
					continue
				}

				if fr.Stream() < sc.lastID {
					sc.writeGoAway(fr.Stream(), ProtocolError, "stream ID is lower than the latest")
					ReleaseFrameHeader(fr)
					continue
				}

				recvWindow := int32(sc.st.MaxWindowSize())

				// Hold streamsMu while reading initialClientWindow and adding stream
				// to prevent race with handleSettings updating it.
				// Note: SETTINGS frames are now processed through the channel in order,
				// so initialClientWindow always reflects the correct value including 0
				// if the client explicitly set it to 0.
				sc.streamsMu.Lock()
				sendWindow := int32(sc.initialClientWindow.Load())
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
					ReleaseFrameHeader(fr)
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
						closeStream(nstrm)

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
				ref := sc.closeRef.Load()
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

			// Normal (non-closing) path: the frame has been fully processed and its
			// body copied out (headers into the request/HPACK table, DATA via
			// AppendBody), so return it to the pool instead of leaking it to GC.
			ReleaseFrameHeader(fr)
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

func buildGoAwayFrame(strm uint32, code ErrorCode, message string) *FrameHeader {
	ga := AcquireFrame(FrameGoAway).(*GoAway)
	fr := AcquireFrameHeader()

	ga.SetStream(strm)
	ga.SetCode(code)
	ga.SetDataString(message)

	fr.SetBody(ga)
	return fr
}

func (sc *serverConn) writeGoAway(strm uint32, code ErrorCode, message string) {
	fr := buildGoAwayFrame(strm, code, message)
	sc.enqueueFrame(fr)

	if strm != 0 {
		sc.closeRef.Store(sc.lastID)
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
	fr := buildGoAwayFrame(strm, code, message)
	sent := sc.enqueueFrameWithTimeout(fr, timeout)

	if sent && strm != 0 {
		sc.closeRef.Store(sc.lastID)
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
		if strm != nil {
			sc.writeReset(strm.ID(), InternalError)
			strm.SetState(StreamStateClosed)
		} else {
			sc.writeGoAway(0, InternalError, err.Error())
		}
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
		if strm != nil {
			sc.writeReset(strm.ID(), streamErr.Code())
		} else {
			sc.writeGoAway(0, streamErr.Code(), streamErr.Error())
		}
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
		} // Push promise state transitions handled in sendPushPromise
	case StreamStateReserved:
		// Reserved streams are created by PUSH_PROMISE and transition to
		// HalfClosed(local) when the server sends headers.
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

			requestEnds := fr.Flags().Has(FlagEndStream) || strm.pendingEndStream
			if requestEnds {
				if err := validateContentLengthState(strm, true); err != nil {
					return err
				}
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
		bodyLen := len(data.Data())

		if err := sc.consumeStreamWindow(strm, payloadLen); err != nil {
			// Stream is reset, but the connection window still owes these bytes.
			sc.queueConnWindowUpdate(payloadLen)
			return err
		}

		strm.bodyBytesReceived += int64(bodyLen)
		if err := validateContentLengthState(strm, fr.Flags().Has(FlagEndStream)); err != nil {
			sc.queueConnWindowUpdate(payloadLen)
			return err
		}

		strm.ctx.Request.AppendBody(data.Data())
		sc.queueWindowUpdate(strm, payloadLen)
	case FrameResetStream:
		if strm.State() == StreamStateIdle {
			return NewGoAwayError(ProtocolError, "RST_STREAM on idle stream")
		}

		// RST flood detection: too many RST_STREAM frames in a short window is
		// a sign of a rapid stream-open/cancel attack. Close the connection with
		// ENHANCE_YOUR_CALM when the rate exceeds the threshold.
		sc.rstMu.Lock()
		now := time.Now()
		if sc.rstWindowStart.IsZero() || now.Sub(sc.rstWindowStart) >= rstRateWindow {
			sc.rstWindowStart = now
			sc.rstCount = 0
		}
		sc.rstCount++
		rstOver := sc.rstCount > rstRateMax
		sc.rstMu.Unlock()

		if rstOver {
			return NewGoAwayError(EnhanceYourCalm, "RST_STREAM flood detected")
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
	if strm.headersFinished {
		// Once the initial request header block is complete, the only valid
		// follow-up HEADERS frame is a trailing header block that MUST carry
		// END_STREAM (RFC 7540 §8.1).
		if !fr.Flags().Has(FlagEndStream) {
			return NewGoAwayError(ProtocolError, "HEADERS without END_STREAM after request headers")
		}
		strm.isTrailer = true
		// Reset the per-block list-size counter for the trailer block.
		strm.headerListSize = 0
	}

	// Guard against CONTINUATION floods (CVE-2024-27983): limit the number of
	// header-block fragments a single stream may send.
	if strm.headerBlockNum >= maxContinuationFrames {
		return NewGoAwayError(ProtocolError, "too many CONTINUATION frames")
	}

	if headerFrame, ok := fr.Body().(*Headers); ok && headerFrame.Stream() == strm.ID() {
		return NewGoAwayError(ProtocolError, "stream that depends on itself")
	}

	// Enforce the raw header-block size cap *before* HPACK decoding to prevent
	// memory exhaustion from highly compressible header data spread across many
	// CONTINUATION frames.
	incoming := fr.Body().(FrameWithHeaders).Headers()
	accumulated := len(strm.previousHeaderBytes) + len(incoming)
	if uint32(accumulated) > maxRawHeaderBlockSize {
		return NewGoAwayError(ProtocolError, "header block size exceeds maximum")
	}

	// Common case: no CONTINUATION leftover, so decode straight from the frame body
	// (each field is copied out into the request and any leftover is copied into
	// previousHeaderBytes below before the frame is released), avoiding an
	// allocation plus full header-block copy on every request.
	var b []byte
	if len(strm.previousHeaderBytes) == 0 {
		b = incoming
	} else {
		b = append(strm.previousHeaderBytes, incoming...)
		strm.previousHeaderBytes = strm.previousHeaderBytes[:0]
	}

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)
	req := &strm.ctx.Request

	var err error

	fieldsProcessed := 0

	// maxHeaderListSize is what we advertised to the client in SETTINGS.
	maxHeaderListSize := sc.st.MaxHeaderListSize()

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

		// RFC 7541 §4.1: track uncompressed header list size and enforce the
		// SETTINGS_MAX_HEADER_LIST_SIZE limit we advertised to the peer.
		strm.headerListSize += uint32(len(hf.key)) + uint32(len(hf.value)) + 32
		if maxHeaderListSize > 0 && strm.headerListSize > maxHeaderListSize {
			return NewGoAwayError(CompressionError, "header list size exceeds SETTINGS_MAX_HEADER_LIST_SIZE")
		}

		k, v := hf.KeyBytes(), hf.ValueBytes()
		if !isValidHTTP2HeaderName(k) {
			return NewResetStreamError(ProtocolError, "invalid header field name")
		}

		if hf.IsPseudo() {
			// Pseudo-headers MUST NOT appear in trailers (RFC 7540 §8.1.2.1).
			if strm.isTrailer {
				return NewResetStreamError(ProtocolError, "pseudo-header in trailer")
			}

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

				if len(v) == 0 {
					return NewResetStreamError(ProtocolError, ":path pseudo-header must not be empty")
				}
				if v[0] != '/' && !(len(v) == 1 && v[0] == '*') {
					return NewResetStreamError(ProtocolError, ":path pseudo-header must be origin-form")
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
			case bytes.Equal(k, StringContentLength):
				contentLength, parseErr := parseContentLength(v)
				if parseErr != nil {
					return NewResetStreamError(ProtocolError, "invalid content-length header")
				}
				if strm.contentLength >= 0 && strm.contentLength != contentLength {
					return NewResetStreamError(ProtocolError, "conflicting content-length header")
				}
				if contentLength > int64(math.MaxInt) {
					return NewResetStreamError(ProtocolError, "invalid content-length header")
				}
				strm.contentLength = contentLength
				req.Header.SetBytesKV(k, v)
			default:
				req.Header.AddBytesKV(k, v)
			}
		}

		fieldsProcessed++
	}

	strm.headerBlockNum++

	if err == nil && fr.Flags().Has(FlagEndHeaders) && len(strm.previousHeaderBytes) == 0 {
		// Reset list-size counter for the next header block.
		strm.headerListSize = 0

		if strm.isTrailer {
			// Trailers do not need pseudo-header validation; we are done.
			return nil
		}

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

func validateContentLengthState(strm *Stream, endStream bool) error {
	if strm.contentLength < 0 {
		return nil
	}
	if strm.bodyBytesReceived > strm.contentLength {
		return NewResetStreamError(ProtocolError, "received body exceeds declared content-length")
	}
	if endStream && strm.bodyBytesReceived != strm.contentLength {
		return NewResetStreamError(ProtocolError, "received body size does not match declared content-length")
	}
	return nil
}

func isValidHTTP2HeaderName(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	if len(name) == 1 && name[0] == ':' {
		return false
	}
	start := 0
	if name[0] == ':' {
		start = 1
	}
	for _, ch := range name[start:] {
		if !validHeaderNameChar[ch] {
			return false
		}
	}
	return true
}

var (
	forbiddenConnection      = []byte("connection")
	forbiddenProxyConnection = []byte("proxy-connection")
	forbiddenTransferEnc     = []byte("transfer-encoding")
	forbiddenUpgrade         = []byte("upgrade")
	forbiddenKeepAlive       = []byte("keep-alive")
	forbiddenTE              = []byte("te")
	teTrailers               = []byte("trailers")
)

// isForbiddenHeader checks if a header is forbidden in HTTP/2.
// HTTP/2 header names are always lowercase, so we use bytes.Equal
// and dispatch on length to minimize comparisons.
func isForbiddenHeader(key, value []byte) (bool, string) {
	switch len(key) {
	case 2: // te
		if bytes.Equal(key, forbiddenTE) {
			if !bytes.EqualFold(bytes.TrimSpace(value), teTrailers) {
				return true, "te"
			}
		}
	case 7: // upgrade
		if bytes.Equal(key, forbiddenUpgrade) {
			return true, "upgrade"
		}
	case 10: // connection, keep-alive
		if bytes.Equal(key, forbiddenConnection) {
			return true, "connection"
		}
		if bytes.Equal(key, forbiddenKeepAlive) {
			return true, "keep-alive"
		}
	case 16: // proxy-connection
		if bytes.Equal(key, forbiddenProxyConnection) {
			return true, "proxy-connection"
		}
	case 17: // transfer-encoding
		if bytes.Equal(key, forbiddenTransferEnc) {
			return true, "transfer-encoding"
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

// TrailerUserValueKey is the key used to store response trailers
// in the RequestCtx.UserValue. Handlers can set trailer headers
// using this key with a map[string]string value:
//
//	ctx.SetUserValue(http2.TrailerUserValueKey, map[string]string{
//	    "grpc-status": "0",
//	    "grpc-message": "",
//	})
const TrailerUserValueKey = "h2-trailers"

// PusherUserValueKey is the key used to access the server push function
// from within a request handler. The value is a func(method, path string)
// that sends a PUSH_PROMISE to the client:
//
//	push := ctx.UserValue(http2.PusherUserValueKey).(func(string, string))
//	push("GET", "/static/style.css")
//
// Push is only available when the client has not disabled push via
// SETTINGS_ENABLE_PUSH=0.
const PusherUserValueKey = "h2-pusher"

// sendPushPromise sends a PUSH_PROMISE frame on the parent stream,
// advertising a server push for the given method and path.
func (sc *serverConn) sendPushPromise(parentStrm *Stream, method, path string) {
	promisedID := sc.nextServerStreamID()

	fr := AcquireFrameHeader()
	fr.SetStream(parentStrm.ID())

	pp := AcquireFrame(FramePushPromise).(*PushPromise)
	pp.SetStream(promisedID)
	pp.SetEndHeaders(true)
	pp.SetPadding(false)

	// Encode the promised request pseudo-headers into the header block
	hf := AcquireHeaderField()
	var headerBuf []byte

	sc.encMu.Lock()
	hf.SetBytes(StringMethod, []byte(method))
	headerBuf = sc.enc.AppendHeader(headerBuf, hf, true)

	hf.SetBytes(StringPath, []byte(path))
	headerBuf = sc.enc.AppendHeader(headerBuf, hf, true)

	hf.SetBytes(StringScheme, parentStrm.scheme)
	headerBuf = sc.enc.AppendHeader(headerBuf, hf, true)

	host := parentStrm.ctx.Request.Header.Peek("Host")
	if len(host) > 0 {
		hf.SetBytes(StringAuthority, host)
		headerBuf = sc.enc.AppendHeader(headerBuf, hf, true)
	}
	sc.encMu.Unlock()

	ReleaseHeaderField(hf)
	pp.SetHeader(headerBuf)

	fr.SetBody(pp)
	sc.enqueueFrame(fr)
}

// nextServerStreamID returns the next even-numbered stream ID for server push.
// Uses a monotonically increasing atomic counter to avoid ID reuse.
func (sc *serverConn) nextServerStreamID() uint32 {
	for {
		cur := sc.nextPushID.Load()
		next := max(cur+2, 2)
		if sc.nextPushID.CompareAndSwap(cur, next) {
			return next
		}
	}
}

// handleEndRequest dispatches the finished request to the handler.
func (sc *serverConn) handleEndRequest(strm *Stream) {
	ctx := strm.ctx
	ctx.Request.Header.SetProtocolBytes(StringHTTP2)

	// Provide push capability if the client supports it
	sc.clientSMu.Lock()
	pushEnabled := sc.clientS.Push()
	sc.clientSMu.Unlock()
	if pushEnabled {
		ctx.SetUserValue(PusherUserValueKey, func(method, path string) {
			sc.sendPushPromise(strm, method, path)
		})
	}

	sc.h(ctx)

	hasBody := ctx.Response.IsBodyStream() || len(ctx.Response.Body()) > 0
	trailers, hasTrailers := ctx.UserValue(TrailerUserValueKey).(map[string]string)

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetEndHeaders(true)
	// Only set EndStream on headers if there's no body AND no trailers
	h.SetEndStream(!hasBody && !hasTrailers)

	fr.SetBody(h)

	sc.encMu.Lock()
	fasthttpResponseHeaders(h, &sc.enc, &ctx.Response)
	sc.encMu.Unlock()

	sc.enqueueFrame(fr)
	if !hasBody && !hasTrailers {
		sc.markResponseEnded(strm)
	}

	if hasBody {
		// When trailers follow, the last DATA frame must NOT carry EndStream
		endStreamOnData := !hasTrailers
		if ctx.Response.IsBodyStream() {
			streamWriter := acquireStreamWrite()
			streamWriter.strm = strm
			streamWriter.sc = sc
			streamWriter.writer = sc.writer
			streamWriter.size = int64(ctx.Response.Header.ContentLength())
			_ = ctx.Response.BodyWriteTo(streamWriter)
			releaseStreamWrite(streamWriter)
		} else {
			sc.queueData(strm, ctx.Response.Body(), endStreamOnData)
		}
	}

	if hasTrailers {
		sc.sendTrailers(strm, trailers)
	}
}

// sendTrailers sends a trailing HEADERS frame with EndStream set.
func (sc *serverConn) sendTrailers(strm *Stream, trailers map[string]string) {
	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetEndHeaders(true)
	h.SetEndStream(true)

	fr.SetBody(h)

	hf := AcquireHeaderField()

	// Use a stack buffer for key lowercasing to avoid per-trailer allocations.
	var keyBuf [64]byte

	sc.encMu.Lock()
	for k, v := range trailers {
		// Lowercase the key using a stack buffer when possible.
		var lk []byte
		if len(k) <= len(keyBuf) {
			lk = keyBuf[:len(k)]
		} else {
			lk = make([]byte, len(k))
		}
		copy(lk, k)
		ToLower(lk)

		hf.SetKeyBytes(lk)
		hf.SetValue(v)
		sc.enc.AppendHeaderField(h, hf, false)
	}
	sc.encMu.Unlock()

	ReleaseHeaderField(hf)

	sc.enqueueFrame(fr)
	sc.markResponseEnded(strm)
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
	strm    *Stream
	sc      *serverConn
	writer  chan<- *FrameHeader
	size    int64
	written int64
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

			s.sendFrame(fr)
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
	oldInitWin := sc.initialClientWindow.Load()

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

	sc.clientMaxFrameSize.Store(int64(maxFrameSize))

	delta := newInitWin - oldInitWin

	// Hold streamsMu while updating initialClientWindow and adjusting streams
	// to prevent race with stream creation and queueData
	sc.streamsMu.Lock()
	var idBuf [32]uint32
	streamIDsToFlush := idBuf[:0]
	if len(sc.streams) > len(idBuf) {
		streamIDsToFlush = make([]uint32, 0, len(sc.streams))
	}
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
	sc.initialClientWindow.Store(newInitWin)

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
}

const (
	maxWindowIncrement = 1<<31 - 1
	maxWindowSize      = maxWindowIncrement
)

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
	// Reuse a stack-local slice to avoid per-call heap allocation.
	// Most connections have fewer than 32 concurrent streams.
	var buf [32]*Stream
	streams := buf[:0]
	if len(sc.streams) > len(buf) {
		streams = make([]*Stream, 0, len(sc.streams))
	}
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

		if current+inc > maxWindowSize {
			return current, errWindowSizeOverflow
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

	// No async retry timer is needed here. All code paths that increase the
	// flow-control window (handleSettings for SETTINGS_INITIAL_WINDOW_SIZE,
	// and WINDOW_UPDATE handling for both stream and connection levels) already
	// call flushPendingData after adjusting the window. If the handler queues
	// data after the window was already opened, queueData sees the updated
	// window and sends directly.
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

	maxFrame := int(sc.clientMaxFrameSize.Load())
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
	// Swap the slice out instead of copying to avoid an allocation.
	data := strm.pendingData
	strm.pendingData = nil
	endStream := strm.pendingDataEndStream
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

// queueConnWindowUpdate replenishes only the connection-level receive window.
// Per RFC 7540 §6.9.1 connection flow control applies to every DATA frame
// regardless of the stream's state, so received bytes must be credited back even
// when the stream is reset (stream flow-control or content-length violation);
// otherwise the connection window shrinks permanently on each rejected DATA frame.
func (sc *serverConn) queueConnWindowUpdate(n int) {
	if n <= 0 {
		return
	}

	connInc := sc.windowIncrement(int64(sc.maxWindow), int64(sc.currentWindow), n)
	if connInc > 0 {
		sc.sendWindowIncrement(0, connInc, func(inc int) {
			sc.currentWindow += int32(inc)
		})
	}
}

func (sc *serverConn) queueWindowUpdate(strm *Stream, n int) {
	if n <= 0 || strm == nil {
		return
	}

	sc.queueConnWindowUpdate(n)

	streamInc := sc.windowIncrement(int64(strm.recvWindowSize), atomic.LoadInt64(&strm.window), n)
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
	hf.SetValueBytes(statusCodeToBytes(res.Header.StatusCode()))

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
