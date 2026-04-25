package http2

const FrameContinuation FrameType = 0x9

var (
	_ Frame            = &Continuation{}
	_ FrameWithHeaders = &Continuation{}
)

// Continuation represents the Continuation frame.
//
// Continuation frame can carry raw headers and/or the EndHeaders flag.
//
// https://tools.ietf.org/html/rfc7540#section-6.10
type Continuation struct {
	rawHeaders []byte
	endHeaders bool
}

// Type returns FrameContinuation.
func (c *Continuation) Type() FrameType {
	return FrameContinuation
}

// Reset clears headers and the EndHeaders flag.
func (c *Continuation) Reset() {
	c.endHeaders = false
	c.rawHeaders = c.rawHeaders[:0]
}

// CopyTo copies the continuation state into cc.
func (c *Continuation) CopyTo(cc *Continuation) {
	cc.endHeaders = c.endHeaders
	cc.rawHeaders = append(cc.rawHeaders[:0], c.rawHeaders...)
}

// Headers returns the raw header block fragment bytes.
func (c *Continuation) Headers() []byte {
	return c.rawHeaders
}

// SetEndHeaders sets whether this is the last CONTINUATION frame in the header block.
func (c *Continuation) SetEndHeaders(value bool) {
	c.endHeaders = value
}

// EndHeaders reports whether the END_HEADERS flag is set.
func (c *Continuation) EndHeaders() bool {
	return c.endHeaders
}

// SetHeader replaces the header block fragment with b.
func (c *Continuation) SetHeader(b []byte) {
	c.rawHeaders = append(c.rawHeaders[:0], b...)
}

// AppendHeader appends the contents of b to the header block fragment.
func (c *Continuation) AppendHeader(b []byte) {
	c.rawHeaders = append(c.rawHeaders, b...)
}

// Write appends b to the header block fragment, implementing io.Writer.
func (c *Continuation) Write(b []byte) (int, error) {
	n := len(b)
	c.AppendHeader(b)
	return n, nil
}

// Deserialize reads a CONTINUATION frame from the given frame header payload.
func (c *Continuation) Deserialize(fr *FrameHeader) error {
	c.endHeaders = fr.Flags().Has(FlagEndHeaders)
	c.SetHeader(fr.payload)

	return nil
}

// Serialize writes the CONTINUATION payload into the frame header.
func (c *Continuation) Serialize(fr *FrameHeader) {
	if c.endHeaders {
		fr.SetFlags(
			fr.Flags().Add(FlagEndHeaders))
	}

	fr.setPayload(c.rawHeaders)
}
