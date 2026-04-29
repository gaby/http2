package http2

import (
	"errors"
	"strconv"
)

var (
	StringPath          = []byte(":path")
	StringStatus        = []byte(":status")
	StringAuthority     = []byte(":authority")
	StringScheme        = []byte(":scheme")
	StringMethod        = []byte(":method")
	StringServer        = []byte("server")
	StringContentLength = []byte("content-length")
	StringContentType   = []byte("content-type")
	StringUserAgent     = []byte("user-agent")
	StringGzip          = []byte("gzip")
	StringGET           = []byte("GET")
	StringHEAD          = []byte("HEAD")
	StringPOST          = []byte("POST")
	StringCONNECT       = []byte("CONNECT")
	StringHTTP2         = []byte("HTTP/2")
)

// statusCodeTable maps common HTTP status codes to pre-allocated byte slices,
// avoiding a strconv.FormatInt allocation per response.
var statusCodeTable [600][]byte

func init() {
	// Pre-populate the most common HTTP status codes.
	for _, code := range []int{
		100, 101,
		200, 201, 202, 203, 204, 205, 206,
		300, 301, 302, 303, 304, 305, 307, 308,
		400, 401, 402, 403, 404, 405, 406, 407, 408, 409,
		410, 411, 412, 413, 414, 415, 416, 417, 418, 422, 425, 426, 428, 429, 431, 451,
		500, 501, 502, 503, 504, 505, 511,
	} {
		statusCodeTable[code] = []byte(strconv.Itoa(code))
	}
}

// statusCodeToBytes returns a byte slice for the given HTTP status code.
// For common codes it returns a pre-allocated slice (zero allocation).
// For uncommon codes it falls back to strconv.AppendInt.
func statusCodeToBytes(code int) []byte {
	if code >= 0 && code < len(statusCodeTable) && statusCodeTable[code] != nil {
		return statusCodeTable[code]
	}
	return strconv.AppendInt(nil, int64(code), 10)
}

// ToLower converts ASCII uppercase letters in b to lowercase in-place and returns b.
func ToLower(b []byte) []byte {
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}

	return b
}

var errInvalidContentLength = errors.New("invalid content-length")

// parseContentLength parses a non-negative integer from a byte slice
// without allocating a string. Returns the parsed value and any error.
func parseContentLength(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, errInvalidContentLength
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, errInvalidContentLength
		}
		n = n*10 + int64(c-'0')
		if n < 0 { // overflow
			return 0, errInvalidContentLength
		}
	}
	return n, nil
}

const (
	// H2TLSProto is the string used in ALPN-TLS negotiation.
	H2TLSProto = "h2"
	// H2Clean is the string used in HTTP headers by the client to upgrade the connection.
	H2Clean = "h2c"
)
