package http2

import (
	"container/list"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	DefaultPingInterval    = time.Second * 3
	DefaultMaxResponseTime = time.Minute
)

// ClientOpts defines the client options for the HTTP/2 connection.
type ClientOpts struct {
	// OnRTT is assigned to every client after creation, and the handler
	// will be called after every RTT measurement (after receiving a PONG message).
	OnRTT func(time.Duration)
	// PingInterval defines the interval in which the client will ping the server.
	//
	// An interval of 0 will make the library to use DefaultPingInterval. Because ping intervals can't be disabled.
	PingInterval time.Duration

	// MaxResponseTime defines a timeout to wait for the server's response.
	// If the server doesn't reply within MaxResponseTime the stream will be canceled.
	//
	// If MaxResponseTime is 0, DefaultMaxResponseTime will be used.
	// If MaxResponseTime is <0, the max response timeout check will be disabled.
	MaxResponseTime time.Duration

	// MaxConns limits the maximum number of concurrent HTTP/2 connections
	// the client will open to the host. A new connection is only created when
	// all existing connections are at their max concurrent streams limit.
	//
	// A value of 0 means unlimited connections (the default).
	MaxConns int

	// DialTimeout sets a deadline for the TCP connection and TLS handshake
	// when creating new connections. A value of 0 means no timeout.
	DialTimeout time.Duration

	// WindowSize sets the connection-level flow control window size for
	// new connections. A value of 0 uses the default of 1 MiB (1 << 20).
	// Maximum value is 2^31 - 1 (2147483647).
	WindowSize int32

	// DisablePingChecking disables the unacknowledged PING check.
	// By default, the client closes connections that fail to respond
	// to 3 consecutive PINGs. Set this to true for connections where
	// the server may not respond to PINGs promptly.
	DisablePingChecking bool

	// H2C enables cleartext HTTP/2 (prior knowledge) without TLS.
	// When true, the client connects via plain TCP without TLS negotiation.
	// Use this for internal services or connections behind TLS-terminating proxies.
	H2C bool

	// EnableServerPush enables receiving server push (PUSH_PROMISE) frames.
	// When false (default), the client sends SETTINGS_ENABLE_PUSH=0 to the server.
	EnableServerPush bool
}

// String returns a human-readable representation of the client options.
func (opts ClientOpts) String() string {
	return "ClientOpts{ping=" + opts.PingInterval.String() +
		", maxResp=" + opts.MaxResponseTime.String() +
		", maxConns=" + uitoa(uint64(opts.MaxConns)) +
		", h2c=" + boolStr(opts.H2C) +
		", push=" + boolStr(opts.EnableServerPush) + "}"
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (opts *ClientOpts) sanitize() {
	if opts.MaxResponseTime == 0 {
		opts.MaxResponseTime = DefaultMaxResponseTime
	}

	if opts.PingInterval <= 0 {
		opts.PingInterval = DefaultPingInterval
	}

	if opts.WindowSize < 0 {
		opts.WindowSize = 0 // use default
	} else if opts.WindowSize > maxWindowIncrement {
		opts.WindowSize = maxWindowIncrement
	}
}

// Ctx represents a context for a stream. Every stream is related to a context.
type Ctx struct {
	Request  *fasthttp.Request
	Response *fasthttp.Response
	Err      chan error

	onResolve   func(error)
	resolveOnce sync.Once

	streamID   uint32
	sendWindow int32 // tracked send window for this stream
}

// resolve will resolve the context, meaning that provided an error,
func (ctx *Ctx) resolve(err error) {
	ctx.resolveOnce.Do(func() {
		if ctx.onResolve != nil {
			ctx.onResolve(err)
		}

		select {
		case ctx.Err <- err:
		default:
		}
	})
}

type Client struct {
	d *Dialer

	conns list.List

	opts ClientOpts

	lck sync.Mutex
}

func createClient(d *Dialer, opts ClientOpts) *Client {
	opts.sanitize()

	cl := &Client{
		d:    d,
		opts: opts,
	}

	return cl
}

// Close gracefully closes all connections currently managed by this client
// and clears the client's tracked connection list.
func (cl *Client) Close() {
	cl.lck.Lock()
	defer cl.lck.Unlock()

	for e := cl.conns.Front(); e != nil; e = e.Next() {
		_ = e.Value.(*Conn).Close()
	}
	cl.conns.Init()
}

func (cl *Client) onConnectionDropped(c *Conn) {
	cl.lck.Lock()
	defer cl.lck.Unlock()

	for e := cl.conns.Front(); e != nil; e = e.Next() {
		if e.Value.(*Conn) == c {
			cl.conns.Remove(e)

			_, _, _ = cl.createConn()

			break
		}
	}
}

func (cl *Client) createConn() (*Conn, *list.Element, error) {
	c, err := cl.d.Dial(ConnOpts{
		PingInterval:        cl.opts.PingInterval,
		OnDisconnect:        cl.onConnectionDropped,
		OnRTT:               cl.opts.OnRTT,
		DisablePingChecking: cl.opts.DisablePingChecking,
		WindowSize:          cl.opts.WindowSize,
		EnableServerPush:    cl.opts.EnableServerPush,
	})
	if err != nil {
		return nil, nil, err
	}

	return c, cl.conns.PushFront(c), nil
}

var ErrRequestCanceled = errors.New("request timed out")

// ErrMaxConnsReached is returned when all connections are busy and the
// MaxConns limit has been reached, preventing a new connection from being created.
var ErrMaxConnsReached = errors.New("maximum number of HTTP/2 connections reached")

func (cl *Client) RoundTrip(_ *fasthttp.HostClient, req *fasthttp.Request, res *fasthttp.Response) (retry bool, err error) {
	var c *Conn

	cl.lck.Lock()

	var next *list.Element

	for e := cl.conns.Front(); c == nil; e = next {
		if e != nil {
			c = e.Value.(*Conn)
		} else {
			if cl.opts.MaxConns > 0 && cl.conns.Len() >= cl.opts.MaxConns {
				cl.lck.Unlock()
				return false, ErrMaxConnsReached
			}
			c, e, err = cl.createConn()
			if err != nil {
				cl.lck.Unlock()
				return false, err
			}
		}

		// if we can't open a stream, then move on to the next one.
		if !c.CanOpenStream() {
			c = nil
			next = e.Next()
		}

		// if the connection has been closed, then just remove the connection.
		if c != nil && c.Closed() {
			next = e.Next()
			cl.conns.Remove(e)
			c = nil
		}
	}

	cl.lck.Unlock()

	ch := make(chan error, 1)

	var cancelTimer atomic.Pointer[time.Timer]

	ctx := &Ctx{
		Request:  req,
		Response: res,
		Err:      ch,
	}

	ctx.onResolve = func(error) {
		if timer := cancelTimer.Load(); timer != nil {
			timer.Stop()
		}
	}

	if cl.opts.MaxResponseTime > 0 {
		cancelTimer.Store(time.AfterFunc(cl.opts.MaxResponseTime, func() {
			ctx.resolve(ErrRequestCanceled)
			c.cancel(ctx)
		}))
	}

	c.Write(ctx)

	err = <-ch

	return false, err
}
