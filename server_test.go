package http2

import (
	"bufio"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
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
		t.Run("iteration", func(t *testing.T) {
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
		"Content-Length":        strconv.Itoa(len(msg)),
	})
	h2 := makeHeaders(9, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"Content-Length":        strconv.Itoa(len(msg)),
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
		"Content-Length":        strconv.Itoa(len(msg)),
	})
	h2 := makeHeaders(5, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"Content-Length":        strconv.Itoa(len(msg)),
	})
	h3 := makeHeaders(7, c.enc, false, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"Content-Length":        strconv.Itoa(len(msg)),
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

	for i := 0; i < 2; i++ {
		fr, err := c.readNext()
		require.NoError(t, err)

		require.Equal(t, uint32(3), fr.Stream(), "Expecting update on stream 3")
		require.Equal(t, expect[i], fr.Type(), "Unexpected frame type")
	}

	_, err = c.readNext()
	if err != nil {
		_, ok := err.(*GoAway)
		require.True(t, ok, "expected GoAway error")
	}

	_, err = c.readNext()
	require.Error(t, err, "Expecting error")
}
