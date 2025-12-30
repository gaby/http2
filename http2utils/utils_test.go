package http2utils

import (
	"bytes"
	"fmt"
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

func TestAppendUint32Bytes(t *testing.T) {
	start := []byte{0xFF}
	result := AppendUint32Bytes(start, 0x01020304)
	require.Equal(t, []byte{0xFF, 0x01, 0x02, 0x03, 0x04}, result)
}

func TestAssertEqual(t *testing.T) {
	tb := &recordingTB{T: t, name: "assertion"}

	AssertEqual(tb, 1, 1)
	require.False(t, tb.fatalCalled, "fatal should not be triggered on equal values")

	AssertEqual(tb, "expected", "actual", "different values")
	require.True(t, tb.fatalCalled, "fatal should be triggered on mismatch")
	require.Contains(t, tb.fatalMsg, "Description:")
	require.Contains(t, tb.fatalMsg, "different values")
	require.Contains(t, tb.fatalMsg, "Expect:")
	require.Contains(t, tb.fatalMsg, "expected")
	require.Contains(t, tb.fatalMsg, "Result:")
	require.Contains(t, tb.fatalMsg, "actual")
}

type recordingTB struct {
	*testing.T
	name        string
	fatalCalled bool
	fatalMsg    string
}

func (tb *recordingTB) Name() string { return tb.name }

func (tb *recordingTB) Fatal(args ...any) {
	tb.fatalCalled = true
	tb.fatalMsg = fmt.Sprint(args...)
}

func (tb *recordingTB) Fatalf(format string, args ...any) {
	tb.fatalCalled = true
	tb.fatalMsg = fmt.Sprintf(format, args...)
}
