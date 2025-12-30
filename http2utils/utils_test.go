package http2utils

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUintConversions(t *testing.T) {
	b := make([]byte, 3)
	Uint24ToBytes(b, 0x010203)
	got := BytesToUint24(b)
	require.Equal(t, uint32(0x010203), got, "unexpected uint24")

	b4 := make([]byte, 4)
	Uint32ToBytes(b4, 0x11223344)
	got = BytesToUint32(b4)
	require.Equal(t, uint32(0x11223344), got, "unexpected uint32")
}

func TestEqualsFoldAndResize(t *testing.T) {
	require.True(t, EqualsFold([]byte("GoLang"), []byte("golang")), "expected equals fold")
	require.False(t, EqualsFold([]byte("Go"), []byte("lang")), "unexpected equals fold match")

	resized := Resize(make([]byte, 0, 1), 4)
	require.Len(t, resized, 4)
}

func TestPaddingHelpers(t *testing.T) {
	src := []byte("data")
	padded := AddPadding(src)
	require.Greater(t, len(padded), len(src)+1, "expected extra padding bytes")

	trimmed, err := CutPadding(padded, len(padded))
	require.NoError(t, err)
	require.True(t, bytes.Equal(trimmed, src), "unexpected trimmed payload: %q", trimmed)
}

func TestFastBytesToString(t *testing.T) {
	b := []byte("hello")
	require.Equal(t, "hello", FastBytesToString(b), "unexpected string conversion")
}
