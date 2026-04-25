package http2

import (
	"encoding/binary"
	"time"
)

const FramePing FrameType = 0x6

var _ Frame = &Ping{}

// Ping represents a PING frame (RFC 7540 Section 6.7).
//
// PING frames are used to measure round-trip time and to check if an idle
// connection is still functional. The payload is always exactly 8 bytes.
type Ping struct {
	ack  bool
	data [8]byte
}

// IsAck reports whether this PING is an acknowledgement.
func (p *Ping) IsAck() bool {
	return p.ack
}

// SetAck marks or unmarks this PING as an acknowledgement.
func (p *Ping) SetAck(ack bool) {
	p.ack = ack
}

// Type returns FramePing.
func (p *Ping) Type() FrameType {
	return FramePing
}

// Reset clears the ack flag.
func (p *Ping) Reset() {
	p.ack = false
}

// CopyTo copies the PING state into other.
func (p *Ping) CopyTo(other *Ping) {
	other.ack = p.ack
	other.data = p.data
}

// Write copies b into the PING data field, implementing io.Writer.
func (p *Ping) Write(b []byte) (n int, err error) {
	n = copy(p.data[:], b)
	return
}

// SetData copies b into the 8-byte PING opaque data field.
func (p *Ping) SetData(b []byte) {
	copy(p.data[:], b)
}

// SetCurrentTime stores the current time as nanoseconds in the data field.
func (p *Ping) SetCurrentTime() {
	ts := time.Now().UnixNano()
	binary.BigEndian.PutUint64(p.data[:], uint64(ts))
}

// DataAsTime interprets the 8-byte data field as a UnixNano timestamp.
func (p *Ping) DataAsTime() time.Time {
	return time.Unix(
		0, int64(binary.BigEndian.Uint64(p.data[:])),
	)
}

// Deserialize reads a PING frame from the given frame header payload.
func (p *Ping) Deserialize(frh *FrameHeader) error {
	p.ack = frh.Flags().Has(FlagAck)
	if len(frh.payload) != 8 {
		return NewGoAwayError(FrameSizeError, "invalid ping payload")
	}
	p.SetData(frh.payload)
	return nil
}

// Data returns the 8-byte opaque data payload.
func (p *Ping) Data() []byte {
	return p.data[:]
}

// Serialize writes the PING payload into the frame header.
func (p *Ping) Serialize(fr *FrameHeader) {
	if p.ack {
		fr.SetFlags(fr.Flags().Add(FlagAck))
	}

	fr.setPayload(p.data[:])
}
