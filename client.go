package http2

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	DefaultPingInterval    = time.Second * 3
	DefaultMaxResponseTime = time.Minute
)

// ClientOpts defines the client options for the HTTP/2 connection.
type ClientOpts struct {
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

	// OnRTT is assigned to every client after creation, and the handler
	// will be called after every RTT measurement (after receiving a PONG message).
	OnRTT func(time.Duration)

	// Clock controls time-related operations. If nil, a real clock is used.
	Clock Clock
}

func (opts *ClientOpts) sanitize() {
	if opts.MaxResponseTime == 0 {
		opts.MaxResponseTime = DefaultMaxResponseTime
	}

	if opts.PingInterval <= 0 {
		opts.PingInterval = DefaultPingInterval
	}

	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
}

// Ctx represents a context for a stream. Every stream is related to a context.
type Ctx struct {
	Request  *fasthttp.Request
	Response *fasthttp.Response
	Err      chan error

	streamID    uint32
	resolveOnce sync.Once
	onResolve   func(error)
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

	opts ClientOpts

	clock Clock

	lck   sync.Mutex
	conns list.List
}

func createClient(d *Dialer, opts ClientOpts) *Client {
	opts.sanitize()

	cl := &Client{
		d:     d,
		opts:  opts,
		clock: opts.Clock,
	}

	return cl
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
		PingInterval: cl.d.PingInterval,
		OnDisconnect: cl.onConnectionDropped,
	})
	if err != nil {
		return nil, nil, err
	}

	return c, cl.conns.PushFront(c), nil
}

var ErrRequestCanceled = errors.New("request timed out")

func (cl *Client) RoundTrip(_ *fasthttp.HostClient, req *fasthttp.Request, res *fasthttp.Response) (retry bool, err error) {
	var c *Conn

	cl.lck.Lock()

	var next *list.Element

	for e := cl.conns.Front(); c == nil; e = next {
		if e != nil {
			c = e.Value.(*Conn)
		} else {
			c, e, err = cl.createConn()
			if err != nil {
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

	var cancelMu sync.Mutex
	var cancelTimer Timer

	ctx := &Ctx{
		Request:  req,
		Response: res,
		Err:      ch,
	}

	ctx.onResolve = func(error) {
		cancelMu.Lock()
		defer cancelMu.Unlock()

		if cancelTimer != nil {
			cancelTimer.Stop()
			cancelTimer = nil
		}
	}

	if cl.opts.MaxResponseTime > 0 {
		cancelTimer = cl.clock.AfterFunc(cl.opts.MaxResponseTime, func() {
			ctx.resolve(ErrRequestCanceled)
			c.cancel(ctx)
		})
	}

	c.Write(ctx)

	select {
	case err = <-ch:
	}

	return false, err
}
