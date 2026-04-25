package http2

import (
	"github.com/dgrr/http2/http2utils"
)

const FrameWindowUpdate FrameType = 0x8

var _ Frame = &WindowUpdate{}

// WindowUpdate represents a WINDOW_UPDATE frame (RFC 7540 Section 6.9).
//
// WINDOW_UPDATE frames are used to implement flow control.
// They inform the peer that the sender is ready to receive additional data.
type WindowUpdate struct {
	increment int
}

// Type returns FrameWindowUpdate.
func (wu *WindowUpdate) Type() FrameType {
	return FrameWindowUpdate
}

// Reset zeroes the increment value.
func (wu *WindowUpdate) Reset() {
	wu.increment = 0
}

// CopyTo copies the window update state into w.
func (wu *WindowUpdate) CopyTo(w *WindowUpdate) {
	w.increment = wu.increment
}

// Increment returns the window size increment value.
func (wu *WindowUpdate) Increment() int {
	return wu.increment
}

// SetIncrement sets the window size increment.
// It panics if increment is not in the range [1, 2^31-1].
func (wu *WindowUpdate) SetIncrement(increment int) {
	if increment <= 0 || increment > 1<<31-1 {
		panic("invalid window size increment")
	}
	wu.increment = increment
}

// Deserialize reads a WINDOW_UPDATE frame from the given frame header.
func (wu *WindowUpdate) Deserialize(fr *FrameHeader) error {
	if len(fr.payload) != 4 {
		return NewGoAwayError(FrameSizeError, "window_update frame payload must be 4 bytes")
	}

	wu.increment = int(http2utils.BytesToUint32(fr.payload) & (1<<31 - 1))

	return nil
}

// Serialize writes the WINDOW_UPDATE payload into the frame header.
func (wu *WindowUpdate) Serialize(fr *FrameHeader) {
	fr.payload = http2utils.AppendUint32Bytes(
		fr.payload[:0], uint32(wu.increment))
	fr.length = 4
}
