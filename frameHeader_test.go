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

	bf := bytes.NewBuffer(nil)
	bw := bufio.NewWriter(bf)
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

func TestReadFrameFromShortRead(t *testing.T) {
	// Feed only 5 bytes when 9 are needed for the header
	bf := bytes.NewBuffer([]byte{0, 0, 0, 0, 0})
	br := bufio.NewReader(bf)

	fr, err := ReadFrameFrom(br)
	require.Error(t, err)
	require.Nil(t, fr)
}

func TestReadFrameFromUnknownType(t *testing.T) {
	// Build a frame header with unknown type (0x0A > FrameContinuation=0x9)
	// FrameType is int8, so must use value in range [0, 127]
	var h [9]byte
	http2utils.Uint24ToBytes(h[:3], 0) // length = 0
	h[3] = 0x0A                        // type = unknown (10)
	h[4] = 0                           // flags
	// stream = 0

	bf := bytes.NewBuffer(h[:])
	br := bufio.NewReader(bf)

	fr, err := ReadFrameFromWithSize(br, 16384)
	require.ErrorIs(t, err, ErrUnknownFrameType)
	// fr is returned for inspection on unknown type
	_ = fr
}
