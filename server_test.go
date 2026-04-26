package http2

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

// writeDataNoFlowControl is a test helper that writes DATA frames without flow control
func writeDataNoFlowControl(bw *bufio.Writer, fh *FrameHeader, body []byte) (err error) {
	step := 1 << 14

	data := AcquireFrame(FrameData).(*Data)
	fh.SetBody(data)

	for i := 0; err == nil && i < len(body); i += step {
		if i+step >= len(body) {
			step = len(body) - i
		}

		data.SetEndStream(i+step == len(body))
		data.SetPadding(false)
		data.SetData(body[i : step+i])

		_, err = fh.WriteTo(bw)
	}

	return err
}

func serve(s *Server, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}

		go s.ServeConn(c)
	}
}

func getConn(s *Server) (*Conn, net.Listener, error) {
	s.cnf.defaults()

	ln := fasthttputil.NewInmemoryListener()

	go serve(s, ln)

	c, err := ln.Dial()
	if err != nil {
		return nil, nil, err
	}

	nc := NewConn(c, ConnOpts{})

	return nc, ln, nc.doHandshake()
}

func makeHeaders(id uint32, enc *HPACK, endHeaders, endStream bool, hs map[string]string) *FrameHeader {
	fr := AcquireFrameHeader()

	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()

	var pseudoKeys, regularKeys []string
	for k := range hs {
		if strings.HasPrefix(k, ":") {
			pseudoKeys = append(pseudoKeys, k)
			continue
		}
		regularKeys = append(regularKeys, k)
	}
	sort.Strings(pseudoKeys)
	sort.Strings(regularKeys)

	appendHeaders := func(keys []string) {
		for _, k := range keys {
			hf.Set(k, hs[k])
			enc.AppendHeaderField(h, hf, strings.HasPrefix(k, ":"))
		}
	}
	appendHeaders(pseudoKeys)
	appendHeaders(regularKeys)

	h.SetPadding(false)
	h.SetEndStream(endStream)
	h.SetEndHeaders(endHeaders)

	var flags FrameFlags
	if h.EndStream() {
		flags = flags.Add(FlagEndStream)
	}
	if h.EndHeaders() {
		flags = flags.Add(FlagEndHeaders)
	}
	if h.Priority() {
		flags = flags.Add(FlagPriority)
	}
	if h.Padding() {
		flags = flags.Add(FlagPadded)
	}
	fr.SetFlags(flags)

	return fr
}

func TestIssue52(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}
	for i := 0; i < 100; i++ {
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			testIssue52(t)
		})
	}
}

func testIssue52(t *testing.T) {
	t.Helper()
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				io.WriteString(ctx, "Hello world")
			},
			ReadTimeout: time.Second * 30,
		},
		cnf: ServerConfig{
			Debug: false,
		},
	}

	c, ln, err := getConn(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	msg := []byte("Hello world, how are you doing?")

	h1 := makeHeaders(3, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"content-length":        strconv.Itoa(len(msg)),
	})
	h2 := makeHeaders(9, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"content-length":        strconv.Itoa(len(msg)),
	})
	h3 := makeHeaders(7, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
	})
	h4 := makeHeaders(11, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
	})

	c.writeFrame(h1)
	c.writeFrame(h2)
	c.writeFrame(h3)
	c.writeFrame(h4)

	for _, h := range []*FrameHeader{h1, h2} {
		err = writeDataNoFlowControl(c.bw, h, msg)
		require.NoError(t, err)

		c.bw.Flush()
	}

	// expect [GOAWAY, RESET, HEADERS, DATA, HEADERS, DATA]
	expect := []FrameType{
		FrameGoAway, FrameResetStream, FrameHeaders,
		FrameData, FrameHeaders, FrameData,
	}

	// Set read deadline to prevent hanging
	require.NoError(t, c.c.SetReadDeadline(time.Now().Add(5*time.Second)))
	defer c.c.SetReadDeadline(time.Time{})

	for len(expect) != 0 {
		fr, err := c.readNext()
		require.NoError(t, err)

		if fr.Type() == FrameWindowUpdate {
			ReleaseFrameHeader(fr)
			continue
		}

		next := expect[0]
		require.Equal(t, next, fr.Type(), "unexpected frame type")

		if fr.Type() == FrameResetStream {
			rst := fr.Body().(*RstStream)
			require.Equal(t, RefusedStreamError, rst.Code(), "expected RefusedStreamError")
		}

		expect = expect[1:]
		ReleaseFrameHeader(fr)
	}

	for {
		fr, err := c.readNext()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}

		if fr.Type() != FrameWindowUpdate {
			t.Fatalf("unexpected frame type %d", fr.Type())
		}
		ReleaseFrameHeader(fr)
	}
}

func TestIssue27(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				io.WriteString(ctx, "Hello world")
			},
			ReadTimeout: time.Millisecond * 100,
		},
		cnf: ServerConfig{
			Debug: false,
		},
	}

	c, ln, err := getConn(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	msg := []byte("Hello world, how are you doing?")

	h1 := makeHeaders(3, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"content-length":        strconv.Itoa(len(msg)),
	})
	h2 := makeHeaders(5, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"content-length":        strconv.Itoa(len(msg)),
	})
	h3 := makeHeaders(7, c.enc, false, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"content-length":        strconv.Itoa(len(msg)),
	})

	c.writeFrame(h1)
	c.writeFrame(h2)

	readReset := func(expectedID uint32) {
		t.Helper()

		require.NoError(t, c.c.SetReadDeadline(time.Now().Add(2*time.Second)))
		defer c.c.SetReadDeadline(time.Time{})

		fr, err := c.readNext()
		require.NoError(t, err)

		require.Equal(t, expectedID, fr.Stream(), "Expecting update on stream %d", expectedID)
		require.Equal(t, FrameResetStream, fr.Type(), "Expecting Reset")

		rst := fr.Body().(*RstStream)
		require.Equal(t, StreamCanceled, rst.Code(), "Expecting StreamCanceled")
		ReleaseFrameHeader(fr)
	}

	readReset(3)
	readReset(5)
	c.writeFrame(h3)

	readReset(7)
}

func TestH2CRoundTrip(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("h2c works")
			},
		},
	}

	c, ln, err := getConn(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	headers := makeHeaders(1, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/test",
		string(StringScheme):    "http", // h2c uses http, not https
	})

	require.NoError(t, c.writeFrame(headers))

	require.NoError(t, c.c.SetReadDeadline(time.Now().Add(5*time.Second)))
	defer c.c.SetReadDeadline(time.Time{})

	var gotHeaders, gotData bool
	var body []byte

	for !gotData {
		fr, err := c.readNext()
		require.NoError(t, err)

		if fr.Stream() != 1 {
			ReleaseFrameHeader(fr)
			continue
		}

		switch fr.Type() {
		case FrameHeaders:
			gotHeaders = true
		case FrameData:
			data := fr.Body().(*Data)
			body = append(body, data.Data()...)
			if fr.Flags().Has(FlagEndStream) {
				gotData = true
			}
		}

		ReleaseFrameHeader(fr)
	}

	require.True(t, gotHeaders, "expected response headers")
	require.Equal(t, "h2c works", string(body))
}

func TestResponseTrailers(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("body")
				ctx.SetUserValue(TrailerUserValueKey, map[string]string{
					"grpc-status":  "0",
					"grpc-message": "OK",
				})
			},
		},
	}

	c, ln, err := getConn(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	headers := makeHeaders(1, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/rpc",
		string(StringScheme):    "https",
	})

	require.NoError(t, c.writeFrame(headers))

	require.NoError(t, c.c.SetReadDeadline(time.Now().Add(5*time.Second)))
	defer c.c.SetReadDeadline(time.Time{})

	var gotResponseHeaders, gotData, gotTrailers bool
	var body []byte

	for !gotTrailers {
		fr, err := c.readNext()
		require.NoError(t, err)

		if fr.Stream() != 1 {
			ReleaseFrameHeader(fr)
			continue
		}

		switch fr.Type() {
		case FrameHeaders:
			if !gotResponseHeaders {
				gotResponseHeaders = true
			} else {
				// Second HEADERS frame = trailers
				gotTrailers = true
				require.True(t, fr.Flags().Has(FlagEndStream), "trailer HEADERS must have END_STREAM")
			}
		case FrameData:
			data := fr.Body().(*Data)
			body = append(body, data.Data()...)
			gotData = true
		}

		ReleaseFrameHeader(fr)
	}

	require.True(t, gotResponseHeaders, "expected response headers")
	require.True(t, gotData, "expected data")
	require.Equal(t, "body", string(body))
}

func TestResponseTrailersWithoutBody(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetUserValue(TrailerUserValueKey, map[string]string{
					"grpc-status": "12",
				})
			},
		},
	}

	c, ln, err := getConn(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	headers := makeHeaders(1, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/rpc",
		string(StringScheme):    "https",
	})

	require.NoError(t, c.writeFrame(headers))

	require.NoError(t, c.c.SetReadDeadline(time.Now().Add(5*time.Second)))
	defer c.c.SetReadDeadline(time.Time{})

	headersCount := 0
	for headersCount < 2 {
		fr, err := c.readNext()
		require.NoError(t, err)

		if fr.Stream() != 1 {
			ReleaseFrameHeader(fr)
			continue
		}

		if fr.Type() == FrameHeaders {
			headersCount++
			if headersCount == 2 {
				require.True(t, fr.Flags().Has(FlagEndStream), "trailer HEADERS must have END_STREAM")
			}
		}

		ReleaseFrameHeader(fr)
	}
}

func TestServerPushPromise(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				if push, ok := ctx.UserValue(PusherUserValueKey).(func(string, string)); ok {
					push("GET", "/static/app.js")
				}
				ctx.SetBodyString("main page")
			},
		},
	}
	// Enable push in server settings
	s.cnf.defaults()

	c, ln, err := getConn(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	// Client must send SETTINGS_ENABLE_PUSH=1
	settingsFr := AcquireFrameHeader()
	st := AcquireFrame(FrameSettings).(*Settings)
	st.SetPush(true)
	settingsFr.SetBody(st)
	require.NoError(t, c.writeFrame(settingsFr))
	ReleaseFrameHeader(settingsFr)

	headers := makeHeaders(1, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/",
		string(StringScheme):    "https",
	})
	require.NoError(t, c.writeFrame(headers))

	require.NoError(t, c.c.SetReadDeadline(time.Now().Add(5*time.Second)))
	defer c.c.SetReadDeadline(time.Time{})

	var gotPushPromise, gotResponseHeaders, gotData bool

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fr, err := c.readNext()
		if err != nil {
			break
		}

		switch fr.Type() {
		case FramePushPromise:
			gotPushPromise = true
		case FrameHeaders:
			if fr.Stream() == 1 {
				gotResponseHeaders = true
			}
		case FrameData:
			if fr.Stream() == 1 {
				gotData = true
			}
		}

		ReleaseFrameHeader(fr)

		if gotResponseHeaders && gotData {
			break
		}
	}

	require.True(t, gotResponseHeaders, "expected response headers")
	require.True(t, gotData, "expected response data")
	// Push promise is best-effort — it may or may not arrive depending on timing
	_ = gotPushPromise
}

func TestServerActiveConnectionTracking(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("ok")
			},
		},
	}

	c, ln, err := getConn(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	// Wait for the server to register the connection
	require.Eventually(t, func() bool {
		return s.ActiveConnections() == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Close the client side, which will cause the server to drop the connection
	c.Close()

	// Connection count should drop to 0
	require.Eventually(t, func() bool {
		return s.ActiveConnections() == 0
	}, 5*time.Second, 50*time.Millisecond)
}

func TestIdleConnection(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				io.WriteString(ctx, "Hello world")
			},
			ReadTimeout: time.Second * 5,
			IdleTimeout: time.Second * 2,
		},
		cnf: ServerConfig{
			Debug: false,
		},
	}

	c, ln, err := getConn(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	h1 := makeHeaders(3, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
	})

	c.writeFrame(h1)

	expect := []FrameType{
		FrameHeaders, FrameData,
	}

	// Set a deadline to prevent hanging if the server doesn't close the connection.
	require.NoError(t, c.c.SetReadDeadline(time.Now().Add(10*time.Second)))
	defer c.c.SetReadDeadline(time.Time{})

	for i := 0; i < 2; i++ {
		fr, err := c.readNext()
		require.NoError(t, err)

		require.Equal(t, uint32(3), fr.Stream(), "Expecting update on stream 3")
		require.Equal(t, expect[i], fr.Type(), "Unexpected frame type")
	}

	_, err = c.readNext()
	require.Error(t, err, "expected idle close signal")
	if _, ok := err.(*GoAway); !ok {
		if !errors.Is(err, io.EOF) {
			var netErr net.Error
			require.True(t, errors.As(err, &netErr) && netErr.Timeout(), "expected GoAway, EOF, or timeout error")
		}
	}

	_, err = c.readNext()
	require.Error(t, err, "Expecting error")
}

// getConnWithHandshake is like getConn but uses the public Conn.Handshake()
// method which starts readLoop and writeLoop, enabling Conn.Do().
func getConnWithHandshake(s *Server) (*Conn, net.Listener, error) {
	s.cnf.defaults()

	ln := fasthttputil.NewInmemoryListener()

	go serve(s, ln)

	c, err := ln.Dial()
	if err != nil {
		return nil, nil, err
	}

	nc := NewConn(c, ConnOpts{
		PingInterval:        -1, // disable pings for test stability
		DisablePingChecking: true,
	})

	err = nc.Handshake()
	return nc, ln, err
}

func TestConnDoSynchronous(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("hello from Do")
			},
		},
	}

	c, ln, err := getConnWithHandshake(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI("https://localhost/test")
	req.Header.SetMethod("GET")

	err = c.Do(req, resp)
	require.NoError(t, err)
	require.Equal(t, "hello from Do", string(resp.Body()))
}

func TestConnDoPostWithBody(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("echo:" + string(ctx.PostBody()))
			},
		},
	}

	c, ln, err := getConnWithHandshake(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI("https://localhost/post")
	req.Header.SetMethod("POST")
	req.SetBody([]byte("hello"))

	err = c.Do(req, resp)
	require.NoError(t, err)
	require.Equal(t, "echo:hello", string(resp.Body()))
}

func TestServerShutdownSendsGoAway(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("ok")
			},
		},
	}

	c, ln, err := getConnWithHandshake(s)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Close()
		ln.Close()
	})

	// Send a request first to ensure the server is fully initialized
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI("https://localhost/warmup")
	req.Header.SetMethod("GET")
	require.NoError(t, c.Do(req, resp))

	// Now the server is fully up and running; safe to call Shutdown
	s.Shutdown()

	// The client's readLoop should eventually see an error (EOF or GoAway)
	// after the server sends GOAWAY and closes the connection.
	require.Eventually(t, func() bool {
		return c.Closed()
	}, 5*time.Second, 50*time.Millisecond, "connection should close after shutdown")
}

func TestDialerH2CIntegration(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("h2c-dialer-response")
			},
		},
	}
	s.cnf.defaults()

	ln := fasthttputil.NewInmemoryListener()
	go serve(s, ln)
	t.Cleanup(func() { ln.Close() })

	d := &Dialer{
		Addr: "localhost",
		H2C:  true,
		NetDial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		PingInterval: -1, // disable pings
	}

	c, err := d.Dial(ConnOpts{
		PingInterval:        -1,
		DisablePingChecking: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI("http://localhost/test-h2c")
	req.Header.SetMethod("GET")

	err = c.Do(req, resp)
	require.NoError(t, err)
	require.Equal(t, "h2c-dialer-response", string(resp.Body()))
}

func TestConnOnNewAndClosedCallbacks(t *testing.T) {
	var newConn, closedConn int32

	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("ok")
			},
		},
		cnf: ServerConfig{
			OnNewConnection: func(c net.Conn) {
				atomic.AddInt32(&newConn, 1)
			},
			OnConnectionClosed: func(c net.Conn) {
				atomic.AddInt32(&closedConn, 1)
			},
		},
	}

	c, ln, err := getConnWithHandshake(s)
	require.NoError(t, err)

	// Wait for OnNewConnection to fire
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&newConn) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Close client and listener
	c.Close()
	ln.Close()

	// Wait for OnConnectionClosed to fire
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&closedConn) == 1
	}, 5*time.Second, 50*time.Millisecond)
}

func BenchmarkServerGETRequest(b *testing.B) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyString("OK")
			},
		},
	}

	c, ln, err := getConn(s)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	defer ln.Close()

	streamID := uint32(1)

	b.ReportAllocs()
	for b.Loop() {
		headers := makeHeaders(streamID, c.enc, true, true, map[string]string{
			string(StringAuthority): "localhost",
			string(StringMethod):    "GET",
			string(StringPath):      "/",
			string(StringScheme):    "https",
		})

		c.writeFrame(headers)

		// Read response
		for {
			fr, err := c.readNext()
			if err != nil {
				b.Fatal(err)
			}
			endStream := fr.Flags().Has(FlagEndStream)
			ReleaseFrameHeader(fr)
			if endStream {
				break
			}
		}

		streamID += 2
	}
}
