package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

// parseUintBytes parses an unsigned integer from a byte slice without
// allocating a string. Returns the parsed value and any error.
func parseUintBytes(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, errors.New("empty number")
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid digit: %c", c)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// windowWaitTimeout is the maximum duration to wait for flow control window credit
// before giving up and returning a flow control–related error.
const windowWaitTimeout = 200 * time.Millisecond

// ConnOpts defines the connection options.
type ConnOpts struct {
	// OnDisconnect is a callback that fires when the Conn disconnects.
	OnDisconnect func(c *Conn)

	// OnRTT is called after each round-trip time measurement when a PONG is received.
	// The duration is the time between sending the PING and receiving the PONG.
	OnRTT func(time.Duration)

	// PingInterval defines the interval in which the client will ping the server.
	//
	// An interval of <=0 will make the library to use DefaultPingInterval. Because ping intervals can't be disabled
	PingInterval time.Duration

	// WindowWaitTimeout is the maximum time to wait for flow-control window
	// credit before returning a FlowControlError to the caller.
	// A value of 0 uses the default of 200 ms.
	WindowWaitTimeout time.Duration

	// DisablePingChecking ...
	DisablePingChecking bool

	// WindowSize sets the connection-level flow control window size.
	// A value of 0 uses the default of 1 MiB (1 << 20).
	// Maximum value is 2^31 - 1.
	WindowSize int32
}

// Handshake performs an HTTP/2 handshake. It sends the preface (if requested),
// followed by a SETTINGS frame and a WINDOW_UPDATE frame for the connection window.
func Handshake(preface bool, bw *bufio.Writer, st *Settings, maxWin int32) error {
	if preface {
		err := WritePreface(bw)
		if err != nil {
			return err
		}
	}

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	// write the settings
	var st2 Settings
	st.CopyTo(&st2)

	fr.SetBody(&st2)

	_, err := fr.WriteTo(bw)
	if err == nil {
		// then send a window update
		fr := AcquireFrameHeader()
		wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
		wu.SetIncrement(int(maxWin))

		fr.SetBody(wu)

		_, err = fr.WriteTo(bw)
		if err == nil {
			err = bw.Flush()
		}

		ReleaseFrameHeader(fr)
	}

	return err
}

// Conn represents a raw HTTP/2 connection over TLS + TCP.
type Conn struct {
	c net.Conn

	lastErr error

	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK
	dec *HPACK

	windowCond *sync.Cond

	in  chan *Ctx
	out chan *FrameHeader

	onDisconnect func(*Conn)
	onRTT        func(time.Duration)

	lastRTT atomic.Int64 // nanoseconds

	done chan struct{} // closed when writeLoop exits

	reqQueued sync.Map

	current Settings
	serverS Settings

	pingInterval time.Duration

	closed uint64

	windowWaitTimeout time.Duration
	serverSMu         sync.RWMutex

	lastErrMu      sync.RWMutex
	windowCondOnce sync.Once

	windowMu sync.Mutex

	nextID uint32

	serverWindow       int32
	serverStreamWindow int32

	maxWindow     int32
	currentWindow int32

	openStreams int32

	state    connState
	closeRef uint32

	unacks      int32
	disableAcks bool
}

// NewConn returns a new HTTP/2 connection.
// To start using the connection you need to call Handshake.
func NewConn(c net.Conn, opts ConnOpts) *Conn {
	wwt := opts.WindowWaitTimeout
	if wwt <= 0 {
		wwt = windowWaitTimeout
	}
	winSize := opts.WindowSize
	if winSize <= 0 {
		winSize = 1 << 20 // 1 MiB default
	}
	nc := &Conn{
		c:                 c,
		br:                bufio.NewReaderSize(c, 4096),
		bw:                bufio.NewWriterSize(c, maxFrameSize),
		enc:               AcquireHPACK(),
		dec:               AcquireHPACK(),
		nextID:            1,
		serverWindow:      int32(defaultWindowSize), // RFC 7540 default initial window
		maxWindow:         winSize,
		currentWindow:     winSize,
		in:                make(chan *Ctx, 128),
		out:               make(chan *FrameHeader, 128),
		pingInterval:      opts.PingInterval,
		disableAcks:       opts.DisablePingChecking,
		onDisconnect:      opts.OnDisconnect,
		onRTT:             opts.OnRTT,
		windowWaitTimeout: wwt,
	}
	nc.windowCond = sync.NewCond(&nc.windowMu)

	nc.current.SetMaxWindowSize(uint32(winSize))
	nc.current.SetPush(false)

	return nc
}

// Dialer allows creating HTTP/2 connections by specifying an address and tls configuration.
type Dialer struct {
	// TLSConfig is the tls configuration.
	//
	// If TLSConfig is nil, a default one will be defined on the Dial call.
	TLSConfig *tls.Config

	// NetDial defines the callback for establishing new connection to the host.
	// Default Dial is used if not set.
	NetDial fasthttp.DialFunc
	// Addr is the server's address in the form: `host:port`.
	Addr string

	// PingInterval defines the interval in which the client will ping the server.
	//
	// An interval of 0 will make the library to use DefaultPingInterval. Because ping intervals can't be disabled.
	PingInterval time.Duration

	// Timeout sets a deadline for the TCP connection and TLS handshake.
	// A value of 0 means no timeout (the default).
	Timeout time.Duration

	// H2C enables cleartext HTTP/2 (prior knowledge) without TLS.
	// When true, the Dialer connects via plain TCP and skips TLS negotiation.
	H2C bool
}

func (d *Dialer) dialTCP() (net.Conn, error) {
	var c net.Conn
	var err error

	if d.NetDial != nil {
		c, err = d.NetDial(d.Addr)
	} else if d.Timeout > 0 {
		c, err = net.DialTimeout("tcp", d.Addr, d.Timeout)
	} else {
		tcpAddr, err := net.ResolveTCPAddr("tcp", d.Addr)
		if err != nil {
			return nil, err
		}
		c, err = net.DialTCP("tcp", nil, tcpAddr)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return c, err
}

func (d *Dialer) tryDial() (net.Conn, error) {
	if d.H2C {
		return d.dialTCP()
	}

	if d.TLSConfig == nil || !slices.Contains(d.TLSConfig.NextProtos, "h2") {
		configureDialer(d)
	}

	c, err := d.dialTCP()
	if err != nil {
		return nil, err
	}

	if d.Timeout > 0 {
		_ = c.SetDeadline(time.Now().Add(d.Timeout))
	}

	tlsConn := tls.Client(c, d.TLSConfig)

	if err := tlsConn.Handshake(); err != nil {
		_ = c.Close()
		return nil, err
	}

	// Clear the deadline after successful handshake
	if d.Timeout > 0 {
		_ = c.SetDeadline(time.Time{})
	}

	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		_ = c.Close()
		return nil, ErrServerSupport
	}

	return tlsConn, nil
}

// Dial creates an HTTP/2 connection or returns an error.
//
// An expected error is ErrServerSupport.
func (d *Dialer) Dial(opts ConnOpts) (*Conn, error) {
	c, err := d.tryDial()
	if err != nil {
		return nil, err
	}

	nc := NewConn(c, opts)

	err = nc.Handshake()
	return nc, err
}

// SetOnDisconnect sets the callback that will fire when the HTTP/2 connection is closed.
func (c *Conn) SetOnDisconnect(cb func(*Conn)) {
	c.onDisconnect = cb
}

func (c *Conn) setLastErr(err error) {
	c.lastErrMu.Lock()
	c.lastErr = err
	c.lastErrMu.Unlock()
}

func (c *Conn) loadLastErr() error {
	c.lastErrMu.RLock()
	defer c.lastErrMu.RUnlock()
	return c.lastErr
}

// LastErr returns the last registered error in case the connection was closed by the server.
func (c *Conn) LastErr() error {
	return c.loadLastErr()
}

// Handshake will perform the necessary handshake to establish the connection
// with the server. If an error is returned you can assume the TCP connection has been closed.
func (c *Conn) Handshake() error {
	err := c.doHandshake()
	if err == nil {
		c.done = make(chan struct{})
		go c.writeLoop()
		go c.readLoop()
	}

	return err
}

func (c *Conn) doHandshake() error {
	var err error

	if err = Handshake(true, c.bw, &c.current, c.maxWindow-65535); err != nil {
		_ = c.c.Close()
		return err
	}

	var fr *FrameHeader

	if fr, err = ReadFrameFrom(c.br); err == nil && fr.Type() != FrameSettings {
		_ = c.c.Close()
		return fmt.Errorf("unexpected frame, expected settings, got %s", fr.Type())
	} else if err == nil {
		st := fr.Body().(*Settings)
		if !st.IsAck() {
			c.updateServerSettings(st)
			if st.HeaderTableSize() <= defaultHeaderTableSize {
				c.enc.SetMaxTableSize(st.HeaderTableSize())
			}

			// reply back
			fr := AcquireFrameHeader()

			stRes := AcquireFrame(FrameSettings).(*Settings)
			stRes.SetAck(true)

			fr.SetBody(stRes)

			if _, err = fr.WriteTo(c.bw); err == nil {
				err = c.bw.Flush()
			}

			ReleaseFrameHeader(fr)
		}
	}

	if err != nil {
		_ = c.c.Close()
	} else {
		ReleaseFrameHeader(fr)
	}

	return err
}

func (c *Conn) updateServerSettings(st *Settings) {
	c.serverSMu.Lock()
	prevStreamWindow := c.serverS.MaxWindowSize()
	prevWindowExplicit := c.serverS.HasMaxWindowSize()
	if !prevWindowExplicit {
		prevStreamWindow = defaultWindowSize
	}
	st.CopyTo(&c.serverS)
	if !st.HasMaxWindowSize() {
		c.serverS.windowSize = prevStreamWindow
		c.serverS.windowSet = prevWindowExplicit
	}
	newStreamWindow := int32(c.serverS.MaxWindowSize())

	// RFC 7540 Section 6.9.2: When SETTINGS_INITIAL_WINDOW_SIZE changes,
	// adjust all existing stream flow-control windows by the difference
	delta := newStreamWindow - int32(prevStreamWindow)
	if delta != 0 {
		c.reqQueued.Range(func(_, value any) bool {
			ctx := value.(*Ctx)
			atomic.AddInt32(&ctx.sendWindow, delta)
			return true
		})
	}

	c.serverStreamWindow = newStreamWindow
	c.serverSMu.Unlock()

	if delta != 0 {
		c.notifyWindowAvailable()
	}
}

func (c *Conn) windowCondition() *sync.Cond {
	c.windowCondOnce.Do(func() {
		if c.windowCond == nil {
			c.windowCond = sync.NewCond(&c.windowMu)
		}
	})

	return c.windowCond
}

func (c *Conn) notifyWindowAvailable() {
	cond := c.windowCondition()

	c.windowMu.Lock()
	cond.Broadcast()
	c.windowMu.Unlock()
}

// CanOpenStream returns whether the client will be able to open a new stream or not.
func (c *Conn) CanOpenStream() bool {
	c.serverSMu.RLock()
	maxStreams := c.serverS.maxStreams
	c.serverSMu.RUnlock()

	return atomic.LoadInt32(&c.openStreams) < int32(maxStreams)
}

// ActiveStreams returns the number of currently open streams on this connection.
func (c *Conn) ActiveStreams() int32 {
	return atomic.LoadInt32(&c.openStreams)
}

// RTT returns the last measured round-trip time to the server.
// Returns 0 if no PONG has been received yet.
func (c *Conn) RTT() time.Duration {
	return time.Duration(c.lastRTT.Load())
}

// ServerWindow returns the current server-side connection flow control window.
// A value near zero indicates back-pressure from the server.
func (c *Conn) ServerWindow() int32 {
	return atomic.LoadInt32(&c.serverWindow)
}

// MaxConcurrentStreams returns the maximum number of concurrent streams
// allowed by the server on this connection.
func (c *Conn) MaxConcurrentStreams() uint32 {
	c.serverSMu.RLock()
	n := c.serverS.maxStreams
	c.serverSMu.RUnlock()
	return n
}

// RemoteAddr returns the remote network address of the underlying connection.
// Returns nil if the connection is not set.
func (c *Conn) RemoteAddr() net.Addr {
	if c.c == nil {
		return nil
	}
	return c.c.RemoteAddr()
}

// LocalAddr returns the local network address of the underlying connection.
// Returns nil if the connection is not set.
func (c *Conn) LocalAddr() net.Addr {
	if c.c == nil {
		return nil
	}
	return c.c.LocalAddr()
}

// PingInterval returns the interval at which the connection sends PING frames
// to the peer. If the connection was created with a zero or negative interval,
// DefaultPingInterval is used once the write loop starts; this accessor
// reflects the currently configured value.
func (c *Conn) PingInterval() time.Duration {
	if c.pingInterval <= 0 {
		return DefaultPingInterval
	}
	return c.pingInterval
}

// Closed indicates whether the connection is closed or not.
func (c *Conn) Closed() bool {
	return atomic.LoadUint64(&c.closed) == 1
}

// Done returns a channel that is closed when the connection's write loop exits.
// This is useful for select-based event loops that need to react to connection
// teardown. Returns nil if the connection was never fully handshaked.
func (c *Conn) Done() <-chan struct{} {
	return c.done
}

// Close closes the connection gracefully, sending a GoAway message
// and then closing the underlying TCP connection.
//
// Close is safe to call from any goroutine. The GoAway frame and TCP
// teardown are handled by the writeLoop to avoid concurrent writes.
func (c *Conn) Close() error {
	if !atomic.CompareAndSwapUint64(&c.closed, 0, 1) {
		return io.EOF
	}

	c.notifyWindowAvailable()

	if c.in != nil {
		close(c.in)
	}

	// If writeLoop was never started (e.g. Handshake not called, or direct
	// struct construction in tests), handle cleanup directly.
	if c.done == nil {
		if c.c != nil {
			_ = c.c.Close()
		}
		if c.onDisconnect != nil {
			c.onDisconnect(c)
		}
		return nil
	}

	// writeLoop is (or was) running — it handles GoAway + TCP close.
	// Use a backstop to force-close if writeLoop is stuck.
	select {
	case <-c.done:
		// writeLoop already exited and cleaned up
	default:
		go func() {
			select {
			case <-c.done:
			case <-time.After(100 * time.Millisecond):
				if c.c != nil {
					_ = c.c.Close()
				}
			}
		}()
	}

	return nil
}

// Do sends a request and waits for the response synchronously.
// This is a convenience wrapper around Write that handles Ctx creation.
func (c *Conn) Do(req *fasthttp.Request, resp *fasthttp.Response) error {
	ctx := &Ctx{
		Request:  req,
		Response: resp,
		Err:      make(chan error, 1),
	}

	c.Write(ctx)

	return <-ctx.Err
}

// Write queues the request to be sent to the server.
//
// If the connection is already closed, the request is resolved with
// net.ErrClosed instead of panicking.
func (c *Conn) Write(r *Ctx) {
	if atomic.LoadUint64(&c.closed) == 1 {
		r.resolve(ErrConnectionClosed)
		return
	}

	defer func() {
		if recover() != nil {
			r.resolve(ErrConnectionClosed)
		}
	}()

	c.in <- r
}

var ErrStreamNotReady = errors.New("stream hasn't been created")

// Cancel will try to cancel the request.
//
// Cancel can only return ErrStreamNotReady when the cancel is performed before the stream is created.
func (c *Conn) Cancel(ctx *Ctx) error {
	if atomic.LoadUint32(&ctx.streamID) == 0 {
		return ErrStreamNotReady
	}

	c.cancel(ctx)

	return nil
}

func (c *Conn) cancel(ctx *Ctx) {
	h := AcquireFrameHeader()
	h.SetStream(atomic.LoadUint32(&ctx.streamID))

	fr := AcquireFrame(FrameResetStream).(*RstStream)
	fr.SetCode(StreamCanceled)

	h.SetBody(fr)

	c.trySendOut(h)
}

type WriteError struct {
	err error
}

func (we WriteError) Error() string {
	return fmt.Sprintf("writing error: %s", we.err)
}

func (we WriteError) Unwrap() error {
	return we.err
}

func (we WriteError) Is(target error) bool {
	return errors.Is(we.err, target)
}

func (we WriteError) As(target any) bool {
	return errors.As(we.err, target)
}

// trySendOut sends a frame to the outgoing channel without blocking if the
// connection is shutting down. This prevents goroutine leaks when writeLoop
// has already exited.
func (c *Conn) trySendOut(fr *FrameHeader) {
	if c.done == nil {
		// writeLoop was never started; best-effort non-blocking send.
		select {
		case c.out <- fr:
		default:
			ReleaseFrameHeader(fr)
		}
		return
	}
	select {
	case c.out <- fr:
	case <-c.done:
		ReleaseFrameHeader(fr)
	}
}

func (c *Conn) writeLoop() {
	var lastErr error

	// 4. Signal that writeLoop has fully exited (runs last).
	defer func() {
		if c.done != nil {
			close(c.done)
		}
	}()

	// 3. Write GoAway and close the TCP connection (runs third).
	defer func() {
		if c.bw != nil {
			fr := AcquireFrameHeader()
			ga := AcquireFrame(FrameGoAway).(*GoAway)
			ga.SetStream(0)
			ga.SetCode(NoError)
			ga.SetDataString(NoError.String())
			fr.SetBody(ga)
			_, _ = fr.WriteTo(c.bw)
			_ = c.bw.Flush()
			ReleaseFrameHeader(fr)
		}

		if c.c != nil {
			_ = c.c.Close()
		}

		if c.onDisconnect != nil {
			c.onDisconnect(c)
		}
	}()

	// 2. Set the closed flag and close the in channel (runs second).
	defer func() { _ = c.Close() }()

	// 1. Handle panics and resolve all pending requests (runs first).
	defer func() {
		if err := recover(); err != nil {
			if lastErr == nil {
				switch errn := err.(type) {
				case error:
					lastErr = errn
				case string:
					lastErr = errors.New(errn)
				}
			}
		}

		storedErr := c.loadLastErr()
		if lastErr == nil || (storedErr != nil && errors.Is(lastErr, net.ErrClosed)) {
			lastErr = storedErr
		}

		if lastErr == nil {
			lastErr = io.ErrUnexpectedEOF
		}

		c.setLastErr(lastErr)

		c.reqQueued.Range(func(_, v any) bool {
			r := v.(*Ctx)
			r.resolve(lastErr)

			return true
		})
	}()

	if c.pingInterval <= 0 {
		c.pingInterval = DefaultPingInterval
	}

	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()

loop:
	for {
		select {
		case ctx, ok := <-c.in: // sending requests
			if !ok {
				break loop
			}

			err := c.writeRequest(ctx)
			if err != nil {
				ctx.resolve(err)

				if errors.Is(err, ErrNotAvailableStreams) {
					continue
				}

				lastErr = WriteError{err}

				break loop
			}
		case fr, ok := <-c.out: // generic output
			if !ok {
				break loop
			}

			err := c.writeFrame(fr)
			if err != nil {
				lastErr = WriteError{err}
				break loop
			}

			ReleaseFrameHeader(fr)
		case <-ticker.C: // ping
			if err := c.writePing(); err != nil {
				lastErr = WriteError{err}
				break loop
			}
		}

		if !c.disableAcks && atomic.LoadInt32(&c.unacks) >= 3 {
			lastErr = ErrTimeout
			break loop
		}
	}
}

func (c *Conn) writeFrame(fr *FrameHeader) error {
	_, err := fr.WriteTo(c.bw)
	if err == nil {
		if err = c.bw.Flush(); err != nil {
			return err
		}
	}

	return err
}

func (c *Conn) finish(r *Ctx, stream uint32, err error) {
	atomic.AddInt32(&c.openStreams, -1)

	r.resolve(err)

	c.reqQueued.Delete(stream)
}

func (c *Conn) readLoop() {
	defer func() { _ = c.Close() }()

	for {
		fr, err := c.readNext()
		if err != nil {
			c.setLastErr(err)
			break
		}

		if ri, ok := c.reqQueued.Load(fr.Stream()); ok {
			r := ri.(*Ctx)

			err := c.readStream(fr, r.Response)
			if err == nil {
				if fr.Flags().Has(FlagEndStream) {
					c.finish(r, fr.Stream(), nil)
				}
			} else {
				c.finish(r, fr.Stream(), err)

				if errors.Is(err, FlowControlError) {
					break
				}
			}

			if c.state == connStateClosed {
				if fr.Stream() == c.closeRef {
					break
				}
			}
		}

		ReleaseFrameHeader(fr)
	}
}

func (c *Conn) writeRequest(ctx *Ctx) error {
	if !c.CanOpenStream() {
		return ErrNotAvailableStreams
	}

	req := ctx.Request

	hasBody := len(req.Body()) != 0

	enc := c.enc

	id := c.nextID
	c.nextID += 2

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()

	hf.SetBytes(StringAuthority, req.URI().Host())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringMethod, req.Header.Method())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringPath, req.URI().RequestURI())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringScheme, req.URI().Scheme())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringUserAgent, req.Header.UserAgent())
	enc.AppendHeaderField(h, hf, true)

	req.Header.VisitAll(func(k, v []byte) {
		if bytes.EqualFold(k, StringUserAgent) {
			return
		}

		hf.SetBytes(ToLower(k), v)
		enc.AppendHeaderField(h, hf, false)
	})

	h.SetPadding(false)
	h.SetEndStream(!hasBody)
	h.SetEndHeaders(true)

	// store the ctx before sending the request
	atomic.StoreUint32(&ctx.streamID, id)
	// Initialize stream send window and store in map atomically to avoid races with updateServerSettings
	// We need write lock here to ensure we see consistent state with updateServerSettings
	c.serverSMu.Lock()
	streamWin := c.serverStreamWindow
	if streamWin == 0 {
		streamWin = 65535 // RFC 7540 default
	}
	atomic.StoreInt32(&ctx.sendWindow, streamWin)
	c.reqQueued.Store(id, ctx)
	c.serverSMu.Unlock()

	_, err := fr.WriteTo(c.bw)
	if err == nil && hasBody {
		// release headers bc it's going to get replaced by the data frame
		ReleaseFrame(h)

		err = c.writeData(fr, ctx, req.Body())
	}

	if err == nil {
		err = c.bw.Flush()
		if err == nil {
			atomic.AddInt32(&c.openStreams, 1)
		}
	}

	if err != nil {
		c.setLastErr(err)
		// if we had any error, remove it from the reqQueued.
		c.reqQueued.Delete(id)
	}

	ReleaseHeaderField(hf)

	return err
}

func (c *Conn) writeData(fh *FrameHeader, ctx *Ctx, body []byte) (err error) {
	maxFrame := int(defaultDataFrameSize)

	cond := c.windowCondition()
	data := AcquireFrame(FrameData).(*Data)
	fh.SetBody(data)

	wwt := c.windowWaitTimeout
	if wwt <= 0 {
		wwt = windowWaitTimeout
	}

	// Reuse a single AfterFunc timer across iterations to avoid per-wait allocations.
	// The timer calls notifyWindowAvailable which broadcasts on the cond variable
	// to wake up the waiting goroutine.
	waitTimer := time.AfterFunc(time.Hour, c.notifyWindowAvailable)
	waitTimer.Stop()
	defer waitTimer.Stop()

	for i := 0; err == nil && i < len(body); {
		remaining := len(body) - i

		c.windowMu.Lock()
		for {
			if atomic.LoadUint64(&c.closed) == 1 {
				c.windowMu.Unlock()
				return net.ErrClosed
			}

			connWin := atomic.LoadInt32(&c.serverWindow)
			streamWin := atomic.LoadInt32(&ctx.sendWindow)
			if connWin > 0 && streamWin > 0 {
				break
			}

			waitTimer.Reset(wwt)
			cond.Wait()
			if !waitTimer.Stop() {
				connWin = atomic.LoadInt32(&c.serverWindow)
				streamWin = atomic.LoadInt32(&ctx.sendWindow)

				if atomic.LoadUint64(&c.closed) == 1 {
					c.windowMu.Unlock()
					return net.ErrClosed
				}

				if connWin <= 0 || streamWin <= 0 {
					c.windowMu.Unlock()
					return FlowControlError
				}
			}
		}

		connWin := atomic.LoadInt32(&c.serverWindow)
		streamWin := atomic.LoadInt32(&ctx.sendWindow)

		// Calculate chunk size respecting both windows and max frame size
		toSend := min(min(min(remaining, maxFrame), int(connWin)), int(streamWin))

		// Deduct from both windows
		atomic.AddInt32(&c.serverWindow, -int32(toSend))
		atomic.AddInt32(&ctx.sendWindow, -int32(toSend))
		c.windowMu.Unlock()

		data.SetEndStream(i+toSend == len(body))
		data.SetPadding(false)
		data.SetData(body[i : i+toSend])

		_, err = fh.WriteTo(c.bw)

		i += toSend
	}

	return err
}

func (c *Conn) readNext() (fr *FrameHeader, err error) {
loop:
	for err == nil {
		fr, err = ReadFrameFrom(c.br)
		if err != nil {
			break
		}

		if fr.Stream() != 0 {
			break
		}

		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if it has ack, just ignore
				c.handleSettings(st)
			}
		case FrameWindowUpdate:
			win := int32(fr.Body().(*WindowUpdate).Increment())

			atomic.AddInt32(&c.serverWindow, win)
			c.notifyWindowAvailable()
		case FramePing:
			ping := fr.Body().(*Ping)
			if !ping.IsAck() {
				c.handlePing(ping)
			} else {
				atomic.AddInt32(&c.unacks, -1)
				rtt := time.Since(ping.DataAsTime())
				c.lastRTT.Store(int64(rtt))
				if c.onRTT != nil {
					c.onRTT(rtt)
				}
			}
		case FrameGoAway:
			ga := fr.Body().(*GoAway)
			if ga.stream == 0 {
				_ = c.c.Close()
				err = ga
			} else {
				// wait for the streams to complete
				c.closeRef = ga.stream
				c.state = connStateClosed
			}

			break loop
		}

		ReleaseFrameHeader(fr)
	}

	return
}

var ErrTimeout = errors.New("server is not replying to pings")

func (c *Conn) writePing() error {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	_, err := fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
		if err == nil {
			atomic.AddInt32(&c.unacks, 1)
		}
	}

	return err
}

func (c *Conn) handleSettings(st *Settings) {
	c.updateServerSettings(st)
	c.enc.SetMaxTableSize(st.HeaderTableSize())

	// reply back
	fr := AcquireFrameHeader()

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	c.trySendOut(fr)
}

func (c *Conn) handlePing(ping *Ping) {
	// reply back
	fr := AcquireFrameHeader()

	ping.SetAck(true)

	fr.SetBody(ping)

	c.trySendOut(fr)
}

func (c *Conn) readStream(fr *FrameHeader, res *fasthttp.Response) (err error) {
	switch fr.Type() {
	case FrameHeaders, FrameContinuation:
		h := fr.Body().(FrameWithHeaders)
		err = c.readHeader(h.Headers(), res)
	case FrameData:
		data := fr.Body().(*Data)
		bytesConsumed := fr.Len()

		c.currentWindow -= int32(bytesConsumed)
		currentWin := c.currentWindow

		atomic.AddInt32(&c.serverWindow, -int32(bytesConsumed))

		if data.Len() != 0 {
			res.AppendBody(data.Data())
		}

		if bytesConsumed != 0 {
			// let's send the window update
			c.updateWindow(fr.Stream(), bytesConsumed)
		}

		if currentWin < c.maxWindow/2 {
			nValue := c.maxWindow - currentWin

			c.currentWindow = c.maxWindow

			c.updateWindow(0, int(nValue))
		}
	case FrameWindowUpdate:
		// Handle stream-specific window update
		win := int32(fr.Body().(*WindowUpdate).Increment())
		if ri, ok := c.reqQueued.Load(fr.Stream()); ok {
			ctx := ri.(*Ctx)
			atomic.AddInt32(&ctx.sendWindow, win)
			c.notifyWindowAvailable()
		}
	}

	return
}

func (c *Conn) updateWindow(streamID uint32, size int) {
	fr := AcquireFrameHeader()

	fr.SetStream(streamID)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.SetIncrement(size)

	fr.SetBody(wu)

	c.trySendOut(fr)
}

func (c *Conn) readHeader(b []byte, res *fasthttp.Response) error {
	var err error
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	dec := c.dec

	for len(b) > 0 {
		b, err = dec.Next(hf, b)
		if err != nil {
			return err
		}

		if hf.IsPseudo() {
			if hf.KeyBytes()[1] == 's' { // status
				n, err := parseUintBytes(hf.ValueBytes())
				if err != nil {
					return err
				}

				res.SetStatusCode(n)
				continue
			}
		}

		if bytes.Equal(hf.KeyBytes(), StringContentLength) {
			n, _ := parseUintBytes(hf.ValueBytes())
			res.Header.SetContentLength(n)
		} else {
			res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
		}
	}

	return nil
}
