package http2

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadFrameRejectsHighFrameTypeWithoutPanic guards against the signed
// FrameType bug: a wire type byte >= 0x80 used to become negative, bypass the
// "unknown frame type" guard, and index framePools out of range, panicking the
// reader (and crashing the client, whose read loop had no recover).
func TestReadFrameRejectsHighFrameTypeWithoutPanic(t *testing.T) {
	for _, typeByte := range []byte{0x80, 0xAB, 0xFF} {
		// 9-byte frame header: length=0, type=typeByte, flags=0, stream=0.
		header := []byte{0, 0, 0, typeByte, 0, 0, 0, 0, 0}
		br := bufio.NewReader(bytes.NewReader(header))

		fr, err := ReadFrameFrom(br) // must not panic
		require.ErrorIs(t, err, ErrUnknownFrameType)
		require.Nil(t, fr)
	}
}

// TestHPACKIndexOverflowDoesNotPanic guards against the HPACK peek() overflow:
// a crafted indexed field whose integer has bit 63 set used to produce a huge
// positive dynamic-table index that slipped past the index<0 guard and panicked
// on table[index]. It must now be reported as a decode error instead.
func TestHPACKIndexOverflowDoesNotPanic(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// 0xFF: indexed field with a 7-bit prefix of all ones, so continuation bytes
	// follow; nine 0x80 bytes then 0x01 yield index 0x800000000000007f (bit 63 set).
	b := []byte{0xFF, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}

	_, err := hp.Next(hf, b) // must not panic
	require.Error(t, err)
}
