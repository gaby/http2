package http2

import (
	"bufio"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

// defaultMaxHeaderListSize is the default maximum uncompressed header list size
// the server is willing to accept. 32 KiB provides a safe bound against header
// inflation attacks while allowing generous header sets in practice.
const defaultMaxHeaderListSize uint32 = 32 * 1024

// ServerConfig holds the configuration for an HTTP/2 server.
type ServerConfig struct {
	// PingInterval is the interval at which the server will send a
	// ping message to a client.
	//
	// To disable pings set the PingInterval to a negative value.
	PingInterval time.Duration

	// MaxConcurrentStreams is the maximum number of concurrent streams
	// the server will allow per connection.
	// A value of 0 uses the default of 1024.
	MaxConcurrentStreams int

	// Debug is a flag that will allow the library to print debugging information.
	Debug bool

	// MaxHeaderListSize is the maximum size of uncompressed header fields (sum of
	// name + value + 32 bytes per field) that the server accepts per request.
	// A value of 0 uses the default of 32 KiB.
	MaxHeaderListSize uint32

	// MaxFrameSize is the maximum size of a single HTTP/2 frame payload
	// the server is willing to receive from the client.
	// Valid range is 16384 (16 KiB) to 16777215 (16 MiB - 1).
	// A value of 0 uses the default of 16384.
	MaxFrameSize uint32

	// EnqueueTimeout is the maximum duration the server will wait when the
	// internal frame-write queue is full before dropping the frame.
	// A value of 0 uses the default of 2 seconds.
	EnqueueTimeout time.Duration
}

func (sc *ServerConfig) defaults() {
	if sc.PingInterval == 0 {
		sc.PingInterval = time.Second * 10
	}

	if sc.MaxConcurrentStreams <= 0 {
		sc.MaxConcurrentStreams = 1024
	}

	if sc.MaxHeaderListSize == 0 {
		sc.MaxHeaderListSize = defaultMaxHeaderListSize
	}

	if sc.MaxFrameSize == 0 {
		sc.MaxFrameSize = defaultDataFrameSize
	} else if sc.MaxFrameSize < defaultDataFrameSize {
		sc.MaxFrameSize = defaultDataFrameSize
	} else if sc.MaxFrameSize > maxFrameSize {
		sc.MaxFrameSize = maxFrameSize
	}

	if sc.EnqueueTimeout == 0 {
		sc.EnqueueTimeout = defaultEnqueueTimeout
	}
}

// Server defines an HTTP/2 entity that can handle HTTP/2 connections.
type Server struct {
	s *fasthttp.Server

	cnf ServerConfig

	activeConns int64
}

// ActiveConnections returns the number of currently active HTTP/2 connections.
func (s *Server) ActiveConnections() int64 {
	return atomic.LoadInt64(&s.activeConns)
}

// ServeConn starts serving a net.Conn as HTTP/2.
//
// This function will fail if the connection does not support the HTTP/2 protocol.
func (s *Server) ServeConn(c net.Conn) error {
	atomic.AddInt64(&s.activeConns, 1)
	defer atomic.AddInt64(&s.activeConns, -1)
	defer func() { _ = c.Close() }()

	// Bound the TLS/preface handshake to avoid hangs on misbehaving clients.
	// The deadline is cleared after the HTTP/2 connection is fully set up.
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	if !ReadPreface(c) {
		return errors.New("wrong preface")
	}

	sc := &serverConn{
		c:              c,
		h:              s.s.Handler,
		br:             bufio.NewReader(c),
		bw:             bufio.NewWriterSize(c, 1<<14*10),
		lastID:         0,
		writer:         make(chan *FrameHeader, 128),
		reader:         make(chan *FrameHeader, 128),
		maxRequestTime: s.s.ReadTimeout,
		maxIdleTime:    s.s.IdleTimeout,
		pingInterval:   s.cnf.PingInterval,
		logger:         s.s.Logger,
		debug:          s.cnf.Debug,
		enqueueTimeout: s.cnf.EnqueueTimeout,
	}

	// Clear handshake deadline now that the connection is initialized.
	_ = c.SetDeadline(time.Time{})

	if sc.logger == nil {
		sc.logger = logger
	}

	sc.enc.Reset()
	sc.dec.Reset()

	sc.maxWindow = 1 << 22
	sc.currentWindow = sc.maxWindow

	sc.st.Reset()
	sc.st.SetMaxWindowSize(uint32(sc.maxWindow))
	sc.st.SetMaxConcurrentStreams(uint32(s.cnf.MaxConcurrentStreams))
	sc.st.SetMaxHeaderListSize(s.cnf.MaxHeaderListSize)
	if s.cnf.MaxFrameSize != defaultDataFrameSize {
		sc.st.SetMaxFrameSize(s.cnf.MaxFrameSize)
	}

	if err := sc.Handshake(); err != nil {
		return err
	}

	return sc.Serve()
}
