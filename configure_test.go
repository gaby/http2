package http2

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestServerConfigAccessor(t *testing.T) {
	cnf := ServerConfig{
		PingInterval:        5 * time.Second,
		MaxConcurrentStreams: 512,
		Debug:               true,
	}
	s := ConfigureServer(&fasthttp.Server{}, cnf)

	got := s.Config()
	require.Equal(t, 5*time.Second, got.PingInterval)
	require.Equal(t, 512, got.MaxConcurrentStreams)
	require.True(t, got.Debug)

	// Mutating the copy must not affect the server
	got.PingInterval = 99 * time.Second
	require.Equal(t, 5*time.Second, s.Config().PingInterval)
}

func TestConfigureServerHelpers(t *testing.T) {
	s := &fasthttp.Server{}
	s2 := ConfigureServer(s, ServerConfig{})
	require.NotNil(t, s2)
	require.Equal(t, 10*time.Second, s2.cnf.PingInterval)
	require.Equal(t, 1024, s2.cnf.MaxConcurrentStreams)

	tlsCfg := &tls.Config{}
	s3 := ConfigureServerAndConfig(&fasthttp.Server{}, tlsCfg)
	require.NotNil(t, s3)
	require.Contains(t, tlsCfg.NextProtos, H2TLSProto)
}

func TestServerConfigDefaultValues(t *testing.T) {
	cnf := ServerConfig{}
	cnf.defaults()
	require.Equal(t, 10*time.Second, cnf.PingInterval)
	require.Equal(t, 1024, cnf.MaxConcurrentStreams)
	require.Equal(t, defaultMaxHeaderListSize, cnf.MaxHeaderListSize)
	require.Equal(t, defaultDataFrameSize, cnf.MaxFrameSize)
	require.Equal(t, defaultEnqueueTimeout, cnf.EnqueueTimeout)
	require.Equal(t, int32(1<<22), cnf.MaxWindowSize)   // 4 MiB default
	require.Equal(t, 5*time.Second, cnf.HandshakeTimeout)
}

func TestServerConfigMaxWindowSizeClamping(t *testing.T) {
	// Zero → default (1<<22)
	cnf := ServerConfig{}
	cnf.defaults()
	require.Equal(t, int32(1<<22), cnf.MaxWindowSize)

	// Negative → default
	cnf = ServerConfig{MaxWindowSize: -1}
	cnf.defaults()
	require.Equal(t, int32(1<<22), cnf.MaxWindowSize)

	// maxWindowIncrement is already int32 max (2^31-1), so we just test the default clamp works
	cnf = ServerConfig{MaxWindowSize: int32(maxWindowIncrement)}
	cnf.defaults()
	require.Equal(t, int32(maxWindowIncrement), cnf.MaxWindowSize)

	// Valid value → keep
	cnf = ServerConfig{MaxWindowSize: 1 << 20}
	cnf.defaults()
	require.Equal(t, int32(1<<20), cnf.MaxWindowSize)
}

func TestServerConfigMaxFrameSizeClamping(t *testing.T) {
	// Too small: clamp to minimum (defaultDataFrameSize = 16384)
	cnf := ServerConfig{MaxFrameSize: 100}
	cnf.defaults()
	require.Equal(t, defaultDataFrameSize, cnf.MaxFrameSize)

	// Too large: clamp to maximum (maxFrameSize = 16777215)
	cnf = ServerConfig{MaxFrameSize: 1 << 25}
	cnf.defaults()
	require.Equal(t, uint32(maxFrameSize), cnf.MaxFrameSize)

	// Valid value: keep as-is
	cnf = ServerConfig{MaxFrameSize: 1 << 16}
	cnf.defaults()
	require.Equal(t, uint32(1<<16), cnf.MaxFrameSize)
}

func TestConfigureServerH2C(t *testing.T) {
	s := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("h2c works")
		},
	}

	s2 := ConfigureServerH2C(s, ServerConfig{})
	require.NotNil(t, s2)
	// defaults should be applied
	require.Equal(t, 10*time.Second, s2.cnf.PingInterval)
	require.Equal(t, 1024, s2.cnf.MaxConcurrentStreams)
}

func TestConfigureServerH2CCustomConfig(t *testing.T) {
	s := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {},
	}

	cnf := ServerConfig{
		PingInterval:        5 * time.Second,
		MaxConcurrentStreams: 100,
	}
	s2 := ConfigureServerH2C(s, cnf)
	require.NotNil(t, s2)
	require.Equal(t, 5*time.Second, s2.cnf.PingInterval)
	require.Equal(t, 100, s2.cnf.MaxConcurrentStreams)
}

func TestConfigureServerAndConfigWithServerConfig(t *testing.T) {
	s := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {},
	}
	tlsCfg := &tls.Config{}

	cnf := ServerConfig{
		PingInterval:        3 * time.Second,
		MaxConcurrentStreams: 256,
	}
	s2 := ConfigureServerAndConfig(s, tlsCfg, cnf)
	require.NotNil(t, s2)
	require.Equal(t, 3*time.Second, s2.cnf.PingInterval)
	require.Equal(t, 256, s2.cnf.MaxConcurrentStreams)
	require.Contains(t, tlsCfg.NextProtos, H2TLSProto)
}

func TestConfigureServerAndConfigNoServerConfig(t *testing.T) {
	s := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {},
	}
	tlsCfg := &tls.Config{}

	// Calling without optional ServerConfig should use defaults
	s2 := ConfigureServerAndConfig(s, tlsCfg)
	require.NotNil(t, s2)
	require.Equal(t, 10*time.Second, s2.cnf.PingInterval)
	require.Equal(t, 1024, s2.cnf.MaxConcurrentStreams)
}

func TestClientAdapterRoundTrip(t *testing.T) {
	conn := &Conn{
		in:  make(chan *Ctx, 1),
		out: make(chan *FrameHeader, 1),
	}
	conn.serverS.maxStreams = 1

	client := createClient(&Dialer{}, ClientOpts{MaxResponseTime: -1})
	client.conns.PushBack(conn)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	done := make(chan struct{})
	go func() {
		ctx := <-conn.in
		require.Same(t, req, ctx.Request)
		require.Same(t, res, ctx.Response)
		ctx.resolve(nil)
		close(done)
	}()

	adapter := &ClientTransport{client: client}
	retry, err := adapter.RoundTrip(nil, req, res)
	require.False(t, retry)
	require.NoError(t, err)
	<-done
}

func TestClientClose(t *testing.T) {
	// Create a client with stub connections
	conn1 := &Conn{
		in:  make(chan *Ctx, 1),
		out: make(chan *FrameHeader, 1),
		c:   &stubConn{},
	}
	conn2 := &Conn{
		in:  make(chan *Ctx, 1),
		out: make(chan *FrameHeader, 1),
		c:   &stubConn{},
	}

	client := createClient(&Dialer{}, ClientOpts{MaxResponseTime: -1})
	client.conns.PushBack(conn1)
	client.conns.PushBack(conn2)

	require.Equal(t, 2, client.conns.Len())

	client.Close()

	// After Close, the connection list should be empty
	require.Equal(t, 0, client.conns.Len())
}

func TestClientTransportClose(t *testing.T) {
	conn := &Conn{
		in:  make(chan *Ctx, 1),
		out: make(chan *FrameHeader, 1),
		c:   &stubConn{},
	}

	client := createClient(&Dialer{}, ClientOpts{MaxResponseTime: -1})
	client.conns.PushBack(conn)

	transport := &ClientTransport{client: client}
	transport.Close()

	// After Close, the client's connection list should be empty
	require.Equal(t, 0, client.conns.Len())
}

func TestGetClientTransport(t *testing.T) {
	// When transport is a ClientTransport, it should be returned
	cl := createClient(&Dialer{}, ClientOpts{MaxResponseTime: -1})
	ct := &ClientTransport{client: cl}

	hc := &fasthttp.HostClient{
		Transport: ct,
	}

	got := GetClientTransport(hc)
	require.Same(t, ct, got)

	// When transport is nil or something else, return nil
	hc2 := &fasthttp.HostClient{}
	require.Nil(t, GetClientTransport(hc2))
}

func TestConfigureServerAndConfigCustomConfig(t *testing.T) {
	tlsCfg := &tls.Config{}
	cnf := ServerConfig{
		PingInterval:        3 * time.Second,
		MaxConcurrentStreams: 64,
		Debug:               true,
	}

	s := ConfigureServerAndConfig(&fasthttp.Server{}, tlsCfg, cnf)
	require.NotNil(t, s)
	require.Equal(t, 3*time.Second, s.Config().PingInterval)
	require.Equal(t, 64, s.Config().MaxConcurrentStreams)
	require.True(t, s.Config().Debug)
	require.Contains(t, tlsCfg.NextProtos, H2TLSProto)
}

func TestConfigureServerAndConfigDefaultConfig(t *testing.T) {
	tlsCfg := &tls.Config{}
	s := ConfigureServerAndConfig(&fasthttp.Server{}, tlsCfg)
	require.NotNil(t, s)
	require.Equal(t, 10*time.Second, s.Config().PingInterval) // default
	require.Contains(t, tlsCfg.NextProtos, H2TLSProto)
}
