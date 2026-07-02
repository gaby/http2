package http2

import (
	"bufio"
	"errors"
	"net"
	"sync"
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

	// Logger overrides the logger used for HTTP/2 connection logging.
	// If nil, fasthttp.Server.Logger is used (or a default stdout logger).
	Logger fasthttp.Logger

	// OnNewConnection is called when a new HTTP/2 connection is established.
	// The net.Conn parameter is the underlying connection (may be TLS or plain TCP).
	// This callback can be used for logging, metrics, or connection-level setup.
	OnNewConnection func(net.Conn)

	// OnConnectionClosed is called when an HTTP/2 connection is closed.
	// This callback can be used for cleanup and metrics.
	OnConnectionClosed func(net.Conn)
	// PingInterval is the interval at which the server will send a
	// ping message to a client.
	//
	// To disable pings set the PingInterval to a negative value.
	PingInterval time.Duration

	// MaxConcurrentStreams is the maximum number of concurrent streams
	// the server will allow per connection.
	// A value of 0 uses the default of 1024.
	MaxConcurrentStreams int

	// EnqueueTimeout is the maximum duration the server will wait when the
	// internal frame-write queue is full before dropping the frame.
	// A value of 0 uses the default of 2 seconds.
	EnqueueTimeout time.Duration

	// HandshakeTimeout is the maximum time to wait for the HTTP/2 connection
	// preface and settings exchange. A value of 0 uses the default of 5 seconds.
	HandshakeTimeout time.Duration

	// MaxHeaderListSize is the maximum size of uncompressed header fields (sum of
	// name + value + 32 bytes per field) that the server accepts per request.
	// A value of 0 uses the default of 32 KiB.
	MaxHeaderListSize uint32

	// MaxFrameSize is the maximum size of a single HTTP/2 frame payload
	// the server is willing to receive from the client.
	// Valid range is 16384 (16 KiB) to 16777215 (16 MiB - 1).
	// A value of 0 uses the default of 16384.
	MaxFrameSize uint32

	// MaxWindowSize is the connection-level flow control window size.
	// A value of 0 uses the default of 4 MiB (1 << 22).
	// Maximum value is 2^31 - 1 (2147483647).
	MaxWindowSize int32

	// Debug is a flag that will allow the library to print debugging information.
	Debug bool
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

	if sc.HandshakeTimeout <= 0 {
		sc.HandshakeTimeout = 5 * time.Second
	}

	if sc.MaxWindowSize <= 0 {
		sc.MaxWindowSize = 1 << 22 // 4 MiB default
	} else if sc.MaxWindowSize > maxWindowIncrement {
		sc.MaxWindowSize = maxWindowIncrement
	}
}

// Server defines an HTTP/2 entity that can handle HTTP/2 connections.
type Server struct {
	cnf ServerConfig

	s *fasthttp.Server

	conns map[*serverConn]struct{}

	activeConns atomic.Int64

	mu sync.Mutex
}

// Config returns a copy of the server's current configuration.
// Modifying the returned value does not affect the server.
func (s *Server) Config() ServerConfig {
	return s.cnf
}

// ActiveConnections returns the number of currently active HTTP/2 connections.
func (s *Server) ActiveConnections() int64 {
	return s.activeConns.Load()
}

// String returns a summary of the server state for debugging.
func (s *Server) String() string {
	return "Server{conns=" + uitoa(uint64(s.ActiveConnections())) +
		", maxStreams=" + uitoa(uint64(s.cnf.MaxConcurrentStreams)) +
		", ping=" + s.cnf.PingInterval.String() + "}"
}

// Shutdown initiates shutdown of all HTTP/2 connections by sending GOAWAY
// frames and signaling the connections to close. It does not wait for active
// streams to drain, and it does not close the underlying fasthttp.Server —
// call fasthttp.Server.Shutdown for that.
func (s *Server) Shutdown() {
	s.mu.Lock()
	conns := make([]*serverConn, 0, len(s.conns))
	for sc := range s.conns {
		conns = append(conns, sc)
	}
	s.mu.Unlock()

	for _, sc := range conns {
		sc.writeGoAway(0, NoError, "server shutting down")
		sc.signalConnClose()
	}
}

// ServeConn starts serving a net.Conn as HTTP/2.
//
// This function will fail if the connection does not support the HTTP/2 protocol.
func (s *Server) ServeConn(c net.Conn) error {
	s.activeConns.Add(1)
	defer s.activeConns.Add(-1)
	defer func() { _ = c.Close() }()

	// Bound the TLS/preface handshake to avoid hangs on misbehaving clients.
	// The deadline is cleared after the HTTP/2 connection is fully set up.
	_ = c.SetDeadline(time.Now().Add(s.cnf.HandshakeTimeout))

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

	s.mu.Lock()
	if s.conns == nil {
		s.conns = make(map[*serverConn]struct{})
	}
	s.conns[sc] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, sc)
		s.mu.Unlock()
		if s.cnf.OnConnectionClosed != nil {
			s.cnf.OnConnectionClosed(c)
		}
	}()

	if s.cnf.OnNewConnection != nil {
		s.cnf.OnNewConnection(c)
	}

	if sc.logger == nil {
		if s.cnf.Logger != nil {
			sc.logger = s.cnf.Logger
		} else {
			sc.logger = logger
		}
	}

	sc.enc.Reset()
	sc.dec.Reset()

	sc.maxWindow = s.cnf.MaxWindowSize
	// The connection begins with the mandatory 65535-byte base receive window
	// (RFC 7540 §6.9.2) in addition to the maxWindow granted via WINDOW_UPDATE at
	// handshake, so account for both. Otherwise the server under-counts its receive
	// window by 65535 and can raise a spurious FLOW_CONTROL_ERROR when a conforming
	// client sends up to the amount it was actually advertised. Clamp to avoid int32
	// overflow when maxWindow is configured near the 2^31-1 ceiling.
	sc.currentWindow = sc.maxWindow
	if sc.currentWindow <= maxWindowIncrement-65535 {
		sc.currentWindow += 65535
	}

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

	// Clear handshake deadline now that the HTTP/2 connection is fully
	// established (preface read + SETTINGS exchange complete).
	_ = c.SetDeadline(time.Time{})

	return sc.Serve()
}
