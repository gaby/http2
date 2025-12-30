package http2

import (
	"bufio"
	"bytes"
	"io"
	"testing"

	"github.com/dgrr/http2/http2utils"
	"github.com/stretchr/testify/require"
)

const (
	testStr = "make fasthttp great again"
)

func TestFrameWrite(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	data := AcquireFrame(FrameData).(*Data)

	fr.SetBody(data)

	n, err := io.WriteString(data, testStr)
	require.NoError(t, err)
	require.Equal(t, len(testStr), n, "unexpected size")

	var bf = bytes.NewBuffer(nil)
	var bw = bufio.NewWriter(bf)
	fr.WriteTo(bw)
	bw.Flush()

	b := bf.Bytes()
	require.Equal(t, testStr, string(b[9:]), "payload mismatch")
}

func TestFrameRead(t *testing.T) {
	var h [9]byte
	bf := bytes.NewBuffer(nil)
	br := bufio.NewReader(bf)

	http2utils.Uint24ToBytes(h[:3], uint32(len(testStr)))

	n, err := bf.Write(h[:9])
	require.NoError(t, err)
	require.Equal(t, 9, n, "unexpected written bytes")

	n, err = io.WriteString(bf, testStr)
	require.NoError(t, err)
	require.Equal(t, len(testStr), n, "unexpected written bytes")

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	nn, err := fr.ReadFrom(br)
	require.NoError(t, err)
	n = int(nn)
	require.Equal(t, len(testStr)+9, n, "unexpected read bytes")

	require.Equal(t, FrameData, fr.Type(), "unexpected frame type")

	data := fr.Body().(*Data)

	require.Equal(t, testStr, string(data.Data()), "payload mismatch")
}

// TODO: continue
