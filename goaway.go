package http2

import (
	"strconv"

	"github.com/dgrr/http2/http2utils"
)

const FrameGoAway FrameType = 0x7

var _ Frame = &GoAway{}

// GoAway represents a GOAWAY frame (RFC 7540 Section 6.8).
//
// GOAWAY is used to initiate graceful shutdown of a connection or to signal
// serious error conditions. It contains the last peer-initiated stream ID
// that was processed, an error code, and optional debug data.
type GoAway struct {
	data   []byte // additional data
	stream uint32
	code   ErrorCode
}

// Error returns a human-readable representation of the GOAWAY frame.
func (ga *GoAway) Error() string {
	return "stream=" + strconv.FormatUint(uint64(ga.stream), 10) +
		", code=" + ga.code.String() +
		", data=" + string(ga.data)
}

// Type returns FrameGoAway.
func (ga *GoAway) Type() FrameType {
	return FrameGoAway
}

// Reset clears all GOAWAY fields.
func (ga *GoAway) Reset() {
	ga.stream = 0
	ga.code = 0
	ga.data = ga.data[:0]
}

// CopyTo copies the GOAWAY state into other.
func (ga *GoAway) CopyTo(other *GoAway) {
	other.stream = ga.stream
	other.code = ga.code
	other.data = append(other.data[:0], ga.data...)
}

// Copy returns a deep copy of the GOAWAY frame.
func (ga *GoAway) Copy() *GoAway {
	other := new(GoAway)
	other.stream = ga.stream
	other.code = ga.code
	other.data = append(other.data[:0], ga.data...)
	return other
}

// Code returns the GOAWAY error code.
func (ga *GoAway) Code() ErrorCode {
	return ga.code
}

// SetCode sets the GOAWAY error code.
func (ga *GoAway) SetCode(code ErrorCode) {
	ga.code = code & (1<<31 - 1)
}

// Stream returns the last peer-initiated stream ID.
func (ga *GoAway) Stream() uint32 {
	return ga.stream
}

// SetStream sets the last peer-initiated stream ID.
func (ga *GoAway) SetStream(stream uint32) {
	ga.stream = stream & (1<<31 - 1)
}

// Data returns the optional debug data.
func (ga *GoAway) Data() []byte {
	return ga.data
}

// SetData sets the optional debug data.
func (ga *GoAway) SetData(b []byte) {
	ga.data = append(ga.data[:0], b...)
}

// SetDataString sets the optional debug data from a string without
// an intermediate []byte allocation.
func (ga *GoAway) SetDataString(s string) {
	ga.data = append(ga.data[:0], s...)
}

// Deserialize reads a GOAWAY frame from the given frame header payload.
func (ga *GoAway) Deserialize(fr *FrameHeader) (err error) {
	if len(fr.payload) < 8 { // 8 is the min number of bytes
		err = ErrMissingBytes
	} else {
		ga.stream = http2utils.BytesToUint32(fr.payload)
		ga.code = ErrorCode(http2utils.BytesToUint32(fr.payload[4:]))

		if len(fr.payload[8:]) != 0 {
			ga.data = append(ga.data[:0], fr.payload[8:]...)
		}
	}

	return
}

// Serialize writes the GOAWAY payload into the frame header.
func (ga *GoAway) Serialize(fr *FrameHeader) {
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:0], ga.stream)
	fr.payload = http2utils.AppendUint32Bytes(fr.payload[:4], uint32(ga.code))

	fr.payload = append(fr.payload, ga.data...)
}
