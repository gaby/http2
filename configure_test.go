package http2

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

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

	adapter := &clientAdapter{client: client}
	retry, err := adapter.RoundTrip(nil, req, res)
	require.False(t, retry)
	require.NoError(t, err)
	<-done
}
