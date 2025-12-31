package http2

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestClientRoundTripUsesExistingConn(t *testing.T) {
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

	retry, err := client.RoundTrip(nil, req, res)
	require.False(t, retry)
	require.NoError(t, err)
	<-done
}

func TestClientRoundTripTimeoutCancelsStream(t *testing.T) {
	conn := &Conn{
		in:  make(chan *Ctx, 1),
		out: make(chan *FrameHeader, 1),
	}
	conn.serverS.maxStreams = 1

	clock := NewFakeClock(time.Unix(0, 0))
	client := createClient(&Dialer{}, ClientOpts{
		MaxResponseTime: time.Second,
		Clock:           clock,
	})
	client.conns.PushBack(conn)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	errCh := make(chan error, 1)
	go func() {
		_, err := client.RoundTrip(nil, req, res)
		errCh <- err
	}()

	<-conn.in
	clock.Advance(time.Second)
	err := <-errCh
	require.ErrorIs(t, err, ErrRequestCanceled)

	var resetReceived bool
	select {
	case fr := <-conn.out:
		resetReceived = true
		require.Equal(t, FrameResetStream, fr.Type())
		ReleaseFrameHeader(fr)
	default:
	}
	require.True(t, resetReceived, "cancel frame not enqueued")
}

func TestClientRoundTripTimeoutIgnoresLateResponse(t *testing.T) {
	conn := &Conn{
		in:  make(chan *Ctx, 1),
		out: make(chan *FrameHeader, 1),
	}
	conn.serverS.maxStreams = 1

	clock := NewFakeClock(time.Unix(0, 0))
	client := createClient(&Dialer{}, ClientOpts{
		MaxResponseTime: time.Second,
		Clock:           clock,
	})
	client.conns.PushBack(conn)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	panicCh := make(chan any, 1)
	timeoutDone := make(chan struct{})
	ctxReceived := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()

		ctx := <-conn.in
		close(ctxReceived)
		<-timeoutDone

		ctx.resolve(nil)
	}()

	var retry bool
	var err error
	done := make(chan struct{})
	go func() {
		retry, err = client.RoundTrip(nil, req, res)
		close(done)
	}()

	<-ctxReceived
	clock.Advance(time.Second)
	<-done
	require.False(t, retry)
	require.ErrorIs(t, err, ErrRequestCanceled)

	close(timeoutDone)
	wg.Wait()

	select {
	case p := <-panicCh:
		require.Failf(t, "panic triggered", "%v", p)
	default:
	}
}

func TestConfigureClientRemovesH2WhenServerDoesNotSupportIt(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	hc := &fasthttp.HostClient{
		Addr: srv.Listener.Addr().String(),
		TLSConfig: &tls.Config{
			RootCAs: pool,
			NextProtos: []string{
				"http/1.1",
			},
		},
	}

	err := ConfigureClient(hc, ClientOpts{})
	require.ErrorIs(t, err, ErrServerSupport)
	require.NotContains(t, hc.TLSConfig.NextProtos, "h2")
	require.Equal(t, "", hc.TLSConfig.ServerName)
}
