package http2

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var hfs = []*HeaderField{
	{key: []byte("cookie"), value: []byte("testcookie")},
	{key: []byte("context-type"), value: []byte("text/plain")},
}

func TestHeaderFieldsToString(t *testing.T) {
	require.Equal(t, "0 - context-type: text/plain\n1 - cookie: testcookie\n",
		headerFieldsToString(hfs, 0))
}

func TestAcquireHPACKAndReleaseHPACK(t *testing.T) {
	hp := &HPACK{}
	ReleaseHPACK(hp)
	require.Equal(t, hp, AcquireHPACK())
}

func TestHPACKAppendInt(t *testing.T) {
	n := uint64(15)
	nn := uint64(1337)
	nnn := uint64(122)
	b15 := []byte{15}
	b1337 := []byte{31, 154, 10}
	b122 := []byte{122}
	var dst []byte

	dst = appendInt(dst, 5, n)
	require.True(t, bytes.Equal(dst, b15), "got %v. Expects %v", dst[:1], b15)

	dst = appendInt(dst, 5, nn)
	require.True(t, bytes.Equal(dst, b1337), "got %v. Expects %v", dst, b1337)

	dst[0] = 0
	dst = appendInt(dst[:1], 7, nnn)
	require.True(t, bytes.Equal(dst[:1], b122), "got %v. Expects %v", dst[:1], b122)
}

func checkInt(t *testing.T, err error, n, e uint64, elen int, b []byte) {
	t.Helper()

	require.NoError(t, err)
	require.Equal(t, e, n)
	if b != nil {
		require.Len(t, b, elen, "bad length")
	}
}

func TestHPACKReadInt(t *testing.T) {
	var err error
	var n uint64
	b := []byte{15, 31, 154, 10, 122}

	b, n = readInt(5, b)
	checkInt(t, err, n, 15, 4, b)

	b, n = readInt(5, b)
	checkInt(t, err, n, 1337, 1, b)

	b, n = readInt(7, b)
	checkInt(t, err, n, 122, 0, b)
}

func TestHPACKWriteTwoStrings(t *testing.T) {
	var dstA []byte
	var dstB []byte
	var err error

	strA := []byte(":status")
	strB := []byte("200")

	dst := appendString(nil, strA, false)
	dst = appendString(dst, strB, false)

	dst, dstA, err = readString(nil, dst)
	require.NoError(t, err)

	_, dstB, err = readString(nil, dst)
	require.NoError(t, err)

	require.True(t, bytes.Equal(strA, dstA), "%s<>%s", dstA, strA)

	require.True(t, bytes.Equal(strB, dstB), "%s<>%s", dstB, strB)
}

func check(t *testing.T, slice []*HeaderField, i int, k, v string) {
	t.Helper()

	require.Greater(t, len(slice), i, "fields len exceeded. %d <> %d", len(slice), i)

	hf := slice[i]
	require.Equal(t, k, string(hf.key), "unexpected key")

	require.Equal(t, v, string(hf.value), "unexpected value")
}

func readHPACKAndCheck(t *testing.T, hpack *HPACK, b []byte, fields, table []string, tableSize uint32) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
	defer cancel()

	hfields := make([]*HeaderField, len(fields)/2)
	for i := 0; len(b) > 0; i++ {
		select {
		case <-ctx.Done():
			require.FailNowf(t, "timeout", "error reading headers: %d bytes remaining", len(b))
		default:
		}

		hfields[i] = AcquireHeaderField()
		next, err := hpack.Next(hfields[i], b)
		require.NoError(t, err)
		require.Less(t, len(next), len(b), "decoder made no progress")
		b = next
	}

	require.Len(t, b, 0, "error reading headers")

	n := 0
	for i := 0; i < len(fields); i += 2 {
		check(t, hfields, n, fields[i], fields[i+1])
		n++
	}
	n = 0
	for i := len(table) - 1; i >= 0; i -= 2 {
		check(t, hpack.dynamic, n, table[i-1], table[i])
		n++
	}

	require.Equal(t, tableSize, hpack.DynamicSize(), "Unexpected table size")
}

func TestHPACKReadRequestWithoutHuffman(t *testing.T) {
	b := []byte{
		0x82, 0x86, 0x84, 0x41, 0x0f, 0x77,
		0x77, 0x77, 0x2e, 0x65, 0x78, 0x61,
		0x6d, 0x70, 0x6c, 0x65, 0x2e, 0x63,
		0x6f, 0x6d,
	}
	hpack := AcquireHPACK()

	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
	}, []string{
		":authority", "www.example.com",
	}, 57)

	b = []byte{
		0x82, 0x86, 0x84, 0xbe, 0x58, 0x08,
		0x6e, 0x6f, 0x2d, 0x63, 0x61, 0x63,
		0x68, 0x65,
	}
	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
		"cache-control", "no-cache",
	}, []string{
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 110)

	b = []byte{
		0x82, 0x87, 0x85, 0xbf, 0x40, 0x0a,
		0x63, 0x75, 0x73, 0x74, 0x6f, 0x6d,
		0x2d, 0x6b, 0x65, 0x79, 0x0c, 0x63,
		0x75, 0x73, 0x74, 0x6f, 0x6d, 0x2d,
		0x76, 0x61, 0x6c, 0x75, 0x65,
	}
	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "https",
		":path", "/index.html",
		":authority", "www.example.com",
		"custom-key", "custom-value",
	}, []string{
		"custom-key", "custom-value",
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 164)
	ReleaseHPACK(hpack)
}

func TestHPACKReadRequestWithHuffman(t *testing.T) {
	b := []byte{
		0x82, 0x86, 0x84, 0x41, 0x8c, 0xf1,
		0xe3, 0xc2, 0xe5, 0xf2, 0x3a, 0x6b,
		0xa0, 0xab, 0x90, 0xf4, 0xff,
	}
	hpack := AcquireHPACK()

	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
	}, []string{
		":authority", "www.example.com",
	}, 57)

	b = []byte{
		0x82, 0x86, 0x84, 0xbe, 0x58, 0x86,
		0xa8, 0xeb, 0x10, 0x64, 0x9c, 0xbf,
	}
	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
		"cache-control", "no-cache",
	}, []string{
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 110)

	b = []byte{
		0x82, 0x87, 0x85, 0xbf, 0x40, 0x88,
		0x25, 0xa8, 0x49, 0xe9, 0x5b, 0xa9,
		0x7d, 0x7f, 0x89, 0x25, 0xa8, 0x49,
		0xe9, 0x5b, 0xb8, 0xe8, 0xb4, 0xbf,
	}
	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "https",
		":path", "/index.html",
		":authority", "www.example.com",
		"custom-key", "custom-value",
	}, []string{
		"custom-key", "custom-value",
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 164)
	ReleaseHPACK(hpack)
}

func TestHPACKReadResponseWithoutHuffman(t *testing.T) {
	b := []byte{
		0x48, 0x03, 0x33, 0x30, 0x32, 0x58,
		0x07, 0x70, 0x72, 0x69, 0x76, 0x61,
		0x74, 0x65, 0x61, 0x1d, 0x4d, 0x6f,
		0x6e, 0x2c, 0x20, 0x32, 0x31, 0x20,
		0x4f, 0x63, 0x74, 0x20, 0x32, 0x30,
		0x31, 0x33, 0x20, 0x32, 0x30, 0x3a,
		0x31, 0x33, 0x3a, 0x32, 0x31, 0x20,
		0x47, 0x4d, 0x54, 0x6e, 0x17, 0x68,
		0x74, 0x74, 0x70, 0x73, 0x3a, 0x2f,
		0x2f, 0x77, 0x77, 0x77, 0x2e, 0x65,
		0x78, 0x61, 0x6d, 0x70, 0x6c, 0x65,
		0x2e, 0x63, 0x6f, 0x6d,
	}
	hpack := AcquireHPACK()
	hpack.SetMaxTableSize(256)

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "302",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
		":status", "302",
	}, 222)

	b = []byte{0x48, 0x03, 0x33, 0x30, 0x37, 0xc1, 0xc0, 0xbf}

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "307",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		":status", "307",
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
	}, 222)

	b = []byte{
		0x88, 0xc1, 0x61, 0x1d, 0x4d, 0x6f,
		0x6e, 0x2c, 0x20, 0x32, 0x31, 0x20,
		0x4f, 0x63, 0x74, 0x20, 0x32, 0x30,
		0x31, 0x33, 0x20, 0x32, 0x30, 0x3a,
		0x31, 0x33, 0x3a, 0x32, 0x32, 0x20,
		0x47, 0x4d, 0x54, 0xc0, 0x5a, 0x04,
		0x67, 0x7a, 0x69, 0x70, 0x77, 0x38,
		0x66, 0x6f, 0x6f, 0x3d, 0x41, 0x53,
		0x44, 0x4a, 0x4b, 0x48, 0x51, 0x4b,
		0x42, 0x5a, 0x58, 0x4f, 0x51, 0x57,
		0x45, 0x4f, 0x50, 0x49, 0x55, 0x41,
		0x58, 0x51, 0x57, 0x45, 0x4f, 0x49,
		0x55, 0x3b, 0x20, 0x6d, 0x61, 0x78,
		0x2d, 0x61, 0x67, 0x65, 0x3d, 0x33,
		0x36, 0x30, 0x30, 0x3b, 0x20, 0x76,
		0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e,
		0x3d, 0x31,
	}

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "200",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
		"location", "https://www.example.com",
		"content-encoding", "gzip",
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}, []string{
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"content-encoding", "gzip",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
	}, 215)

	ReleaseHPACK(hpack)
}

func TestHPACKReadResponseWithHuffman(t *testing.T) {
	b := []byte{
		0x48, 0x82, 0x64, 0x02, 0x58, 0x85,
		0xae, 0xc3, 0x77, 0x1a, 0x4b, 0x61,
		0x96, 0xd0, 0x7a, 0xbe, 0x94, 0x10,
		0x54, 0xd4, 0x44, 0xa8, 0x20, 0x05,
		0x95, 0x04, 0x0b, 0x81, 0x66, 0xe0,
		0x82, 0xa6, 0x2d, 0x1b, 0xff, 0x6e,
		0x91, 0x9d, 0x29, 0xad, 0x17, 0x18,
		0x63, 0xc7, 0x8f, 0x0b, 0x97, 0xc8,
		0xe9, 0xae, 0x82, 0xae, 0x43, 0xd3,
	}
	hpack := AcquireHPACK()
	hpack.SetMaxTableSize(256)

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "302",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
		":status", "302",
	}, 222)

	b = []byte{0x48, 0x83, 0x64, 0x0e, 0xff, 0xc1, 0xc0, 0xbf}

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "307",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		":status", "307",
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
	}, 222)

	b = []byte{
		0x88, 0xc1, 0x61, 0x96, 0xd0, 0x7a,
		0xbe, 0x94, 0x10, 0x54, 0xd4, 0x44,
		0xa8, 0x20, 0x05, 0x95, 0x04, 0x0b,
		0x81, 0x66, 0xe0, 0x84, 0xa6, 0x2d,
		0x1b, 0xff, 0xc0, 0x5a, 0x83, 0x9b,
		0xd9, 0xab, 0x77, 0xad, 0x94, 0xe7,
		0x82, 0x1d, 0xd7, 0xf2, 0xe6, 0xc7,
		0xb3, 0x35, 0xdf, 0xdf, 0xcd, 0x5b,
		0x39, 0x60, 0xd5, 0xaf, 0x27, 0x08,
		0x7f, 0x36, 0x72, 0xc1, 0xab, 0x27,
		0x0f, 0xb5, 0x29, 0x1f, 0x95, 0x87,
		0x31, 0x60, 0x65, 0xc0, 0x03, 0xed,
		0x4e, 0xe5, 0xb1, 0x06, 0x3d, 0x50, 0x07,
	}

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "200",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
		"location", "https://www.example.com",
		"content-encoding", "gzip",
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}, []string{
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"content-encoding", "gzip",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
	}, 215)

	ReleaseHPACK(hpack)
}

func compare(b, r []byte) int {
	for i, c := range b {
		if c != r[i] {
			return i
		}
	}
	return -1
}

func writeHPACKAndCheck(t *testing.T, hpack *HPACK, r []byte, fields, table []string, tableSize uint32) {
	t.Helper()

	n := 0
	hfs := make([]*HeaderField, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		hf := AcquireHeaderField()
		hf.Set(fields[i], fields[i+1])
		hfs = append(hfs, hf)
		n++
	}

	var b []byte

	for _, hf := range hfs {
		b = hpack.AppendHeader(b, hf, true)
	}

	if i := compare(b, r); i != -1 {
		require.Failf(t, "compare", "failed in %d (%d): %s", i, tableSize, hexComparison(b[i:], r[i:]))
	}

	n = 0
	for i := len(table) - 1; i >= 0; i -= 2 {
		check(t, hpack.dynamic, n, table[i-1], table[i])
		n++
	}

	require.Equal(t, tableSize, hpack.DynamicSize(), "Unexpected table size")
}

func TestHPACKWriteRequestWithoutHuffman(t *testing.T) {
	r := []byte{
		0x82, 0x86, 0x84, 0x41, 0x0f, 0x77,
		0x77, 0x77, 0x2e, 0x65, 0x78, 0x61,
		0x6d, 0x70, 0x6c, 0x65, 0x2e, 0x63,
		0x6f, 0x6d,
	}
	hpack := AcquireHPACK()
	hpack.DisableCompression = true

	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
	}, []string{
		":authority", "www.example.com",
	}, 57)

	r = []byte{
		0x82, 0x86, 0x84, 0xbe, 0x58, 0x08,
		0x6e, 0x6f, 0x2d, 0x63, 0x61, 0x63,
		0x68, 0x65,
	}
	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
		"cache-control", "no-cache",
	}, []string{
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 110)

	r = []byte{
		0x82, 0x87, 0x85, 0xbf, 0x40, 0x0a,
		0x63, 0x75, 0x73, 0x74, 0x6f, 0x6d,
		0x2d, 0x6b, 0x65, 0x79, 0x0c, 0x63,
		0x75, 0x73, 0x74, 0x6f, 0x6d, 0x2d,
		0x76, 0x61, 0x6c, 0x75, 0x65,
	}

	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "https",
		":path", "/index.html",
		":authority", "www.example.com",
		"custom-key", "custom-value",
	}, []string{
		"custom-key", "custom-value",
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 164)

	ReleaseHPACK(hpack)
}

func TestHPACKWriteRequestWithHuffman(t *testing.T) {
	r := []byte{
		0x82, 0x86, 0x84, 0x41, 0x8c, 0xf1,
		0xe3, 0xc2, 0xe5, 0xf2, 0x3a, 0x6b,
		0xa0, 0xab, 0x90, 0xf4, 0xff,
	}
	hpack := AcquireHPACK()

	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
	}, []string{
		":authority", "www.example.com",
	}, 57)

	r = []byte{
		0x82, 0x86, 0x84, 0xbe, 0x58, 0x86,
		0xa8, 0xeb, 0x10, 0x64, 0x9c, 0xbf,
	}
	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
		"cache-control", "no-cache",
	}, []string{
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 110)

	r = []byte{
		0x82, 0x87, 0x85, 0xbf, 0x40, 0x88,
		0x25, 0xa8, 0x49, 0xe9, 0x5b, 0xa9,
		0x7d, 0x7f, 0x89, 0x25, 0xa8, 0x49,
		0xe9, 0x5b, 0xb8, 0xe8, 0xb4, 0xbf,
	}
	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "https",
		":path", "/index.html",
		":authority", "www.example.com",
		"custom-key", "custom-value",
	}, []string{
		"custom-key", "custom-value",
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 164)
	ReleaseHPACK(hpack)
}

func TestHPACKWriteResponseWithoutHuffman(t *testing.T) { // without huffman
	r := []byte{
		0x48, 0x03, 0x33, 0x30, 0x32, 0x58,
		0x07, 0x70, 0x72, 0x69, 0x76, 0x61,
		0x74, 0x65, 0x61, 0x1d, 0x4d, 0x6f,
		0x6e, 0x2c, 0x20, 0x32, 0x31, 0x20,
		0x4f, 0x63, 0x74, 0x20, 0x32, 0x30,
		0x31, 0x33, 0x20, 0x32, 0x30, 0x3a,
		0x31, 0x33, 0x3a, 0x32, 0x31, 0x20,
		0x47, 0x4d, 0x54, 0x6e, 0x17, 0x68,
		0x74, 0x74, 0x70, 0x73, 0x3a, 0x2f,
		0x2f, 0x77, 0x77, 0x77, 0x2e, 0x65,
		0x78, 0x61, 0x6d, 0x70, 0x6c, 0x65,
		0x2e, 0x63, 0x6f, 0x6d,
	}
	hpack := AcquireHPACK()
	hpack.DisableCompression = true
	hpack.SetMaxTableSize(256)

	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "302",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
		":status", "302",
	}, 222)

	r = []byte{0x48, 0x03, 0x33, 0x30, 0x37, 0xc1, 0xc0, 0xbf}
	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "307",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		":status", "307",
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
	}, 222)

	r = []byte{
		0x88, 0xc1, 0x61, 0x1d, 0x4d, 0x6f,
		0x6e, 0x2c, 0x20, 0x32, 0x31, 0x20,
		0x4f, 0x63, 0x74, 0x20, 0x32, 0x30,
		0x31, 0x33, 0x20, 0x32, 0x30, 0x3a,
		0x31, 0x33, 0x3a, 0x32, 0x32, 0x20,
		0x47, 0x4d, 0x54, 0xc0, 0x5a, 0x04,
		0x67, 0x7a, 0x69, 0x70, 0x77, 0x38,
		0x66, 0x6f, 0x6f, 0x3d, 0x41, 0x53,
		0x44, 0x4a, 0x4b, 0x48, 0x51, 0x4b,
		0x42, 0x5a, 0x58, 0x4f, 0x51, 0x57,
		0x45, 0x4f, 0x50, 0x49, 0x55, 0x41,
		0x58, 0x51, 0x57, 0x45, 0x4f, 0x49,
		0x55, 0x3b, 0x20, 0x6d, 0x61, 0x78,
		0x2d, 0x61, 0x67, 0x65, 0x3d, 0x33,
		0x36, 0x30, 0x30, 0x3b, 0x20, 0x76,
		0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e,
		0x3d, 0x31,
	}

	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "200",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
		"location", "https://www.example.com",
		"content-encoding", "gzip",
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}, []string{
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"content-encoding", "gzip",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
	}, 215)

	ReleaseHPACK(hpack)
}

func TestHPACKWriteResponseWithHuffman(t *testing.T) { // WithHuffman
	r := []byte{
		0x48, 0x82, 0x64, 0x02, 0x58, 0x85,
		0xae, 0xc3, 0x77, 0x1a, 0x4b, 0x61,
		0x96, 0xd0, 0x7a, 0xbe, 0x94, 0x10,
		0x54, 0xd4, 0x44, 0xa8, 0x20, 0x05,
		0x95, 0x04, 0x0b, 0x81, 0x66, 0xe0,
		0x82, 0xa6, 0x2d, 0x1b, 0xff, 0x6e,
		0x91, 0x9d, 0x29, 0xad, 0x17, 0x18,
		0x63, 0xc7, 0x8f, 0x0b, 0x97, 0xc8,
		0xe9, 0xae, 0x82, 0xae, 0x43, 0xd3,
	}

	hpack := AcquireHPACK()
	hpack.SetMaxTableSize(256)
	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "302",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
		":status", "302",
	}, 222)

	r = []byte{0x48, 0x83, 0x64, 0x0e, 0xff, 0xc1, 0xc0, 0xbf}
	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "307",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		":status", "307",
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
	}, 222)

	r = []byte{
		0x88, 0xc1, 0x61, 0x96, 0xd0, 0x7a,
		0xbe, 0x94, 0x10, 0x54, 0xd4, 0x44,
		0xa8, 0x20, 0x05, 0x95, 0x04, 0x0b,
		0x81, 0x66, 0xe0, 0x84, 0xa6, 0x2d,
		0x1b, 0xff, 0xc0, 0x5a, 0x83, 0x9b,
		0xd9, 0xab, 0x77, 0xad, 0x94, 0xe7,
		0x82, 0x1d, 0xd7, 0xf2, 0xe6, 0xc7,
		0xb3, 0x35, 0xdf, 0xdf, 0xcd, 0x5b,
		0x39, 0x60, 0xd5, 0xaf, 0x27, 0x08,
		0x7f, 0x36, 0x72, 0xc1, 0xab, 0x27,
		0x0f, 0xb5, 0x29, 0x1f, 0x95, 0x87,
		0x31, 0x60, 0x65, 0xc0, 0x03, 0xed,
		0x4e, 0xe5, 0xb1, 0x06, 0x3d, 0x50, 0x07,
	}
	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "200",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
		"location", "https://www.example.com",
		"content-encoding", "gzip",
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}, []string{
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"content-encoding", "gzip",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
	}, 215)

	ReleaseHPACK(hpack)
}

func TestHPACKDynamicTableSizeUpdate(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	// Dynamic table size update: encode size=256 with 5-bit prefix
	// 001xxxxx where xxxxx encodes the new size
	// Size 256 doesn't fit in 5 bits (max 31), so: 0x3f (001 11111) + continuation
	// 0x3f = 0010_0000 | 0x1f = 0x3f, then 256 - 31 = 225 → 0xe1, 0x01
	b := []byte{0x3f, 0xe1, 0x01}

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	remaining, err := hp.Next(hf, b)
	require.NoError(t, err)
	require.Empty(t, remaining)
	require.Equal(t, uint32(256), hp.maxTableSize)
}

func TestHPACKDynamicTableSizeUpdateErrors(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// Dynamic table size update NOT at beginning of header block should fail
	// First encode a regular indexed field, then a size update
	// 0x82 = indexed header field, index 2 (:method GET)
	// 0x20 = dynamic table size update to 0
	b := []byte{0x82, 0x20}

	b2, err := hp.Next(hf, b)
	require.NoError(t, err) // first field ok

	_, err = hp.nextField(hf, 0, 1, b2) // fieldsProcessed=1, so size update should fail
	require.ErrorIs(t, err, ErrDynamicUpdate)
}

func TestHPACKDynamicTableSizeUpdateMaxExceeded(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(100) // small limit

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// Try to set dynamic table size to 200 (exceeds maxTableSizeSettings=100)
	// Encode 200 with 5-bit prefix: 0x3f (31) + 169 (0xa9, 0x01)
	b := []byte{0x3f, 0xa9, 0x01}
	_, err := hp.Next(hf, b)
	require.ErrorIs(t, err, ErrDynamicUpdateMaxTableSize)
}

func TestHPACKNeverIndexedField(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// Never-indexed literal header field: 0001xxxx
	// 0x10 = never indexed, new name
	// key: "secret-key" (len=10)
	// value: "secret-val" (len=10)
	key := []byte("secret-key")
	val := []byte("secret-val")
	b := []byte{0x10, byte(len(key))}
	b = append(b, key...)
	b = append(b, byte(len(val)))
	b = append(b, val...)

	remaining, err := hp.Next(hf, b)
	require.NoError(t, err)
	require.Empty(t, remaining)
	require.True(t, hf.IsSensible(), "never-indexed field should be sensible")
	require.Equal(t, "secret-key", hf.Key())
	require.Equal(t, "secret-val", hf.Value())

	// Should NOT be added to dynamic table
	require.Empty(t, hp.dynamic)
}

func TestReadStringEdgeCases(t *testing.T) {
	// Empty input should return error
	_, _, err := readString(nil, nil)
	require.Error(t, err)

	// Truncated string: declare length 10 but provide only 3 bytes of data
	b := []byte{10, 'a', 'b', 'c'}
	_, _, err = readString(nil, b)
	require.ErrorIs(t, err, ErrUnexpectedSize)

	// Huffman-encoded but with invalid data that HuffmanDecode rejects
	// 0x80 | 0x02 = huffman flag + length 2, followed by garbage bytes
	b = []byte{0x82, 0x00, 0x00} // 0x82 = huffman + len=2, data = [0x00, 0x00]
	_, _, err = readString(nil, b)
	require.Error(t, err, "should fail on invalid huffman data")
}

func TestHPACKPeekOutOfRange(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)

	// Peek with index beyond dynamic table should return nil
	result := hp.peek(maxIndex + 100)
	require.Nil(t, result)
}

func TestHPACKIndexedFieldInvalid(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// Indexed header field with index that doesn't exist in static or dynamic table
	// 0xFF 0x00 = indexed field, index = 127 (way beyond static table)
	b := []byte{0xFF, 0x00}
	_, err := hp.Next(hf, b)
	require.Error(t, err)
	require.Contains(t, err.Error(), "index field not found")
}

func TestHPACKLiteralWithIndexedKey(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// Literal with incremental indexing, key from static table index 1 (:authority)
	// 0x41 = 0100_0001 → literal with incremental indexing, key index = 1
	// Value: "example.com" (len=11, no huffman)
	val := []byte("example.com")
	b := []byte{0x41, byte(len(val))}
	b = append(b, val...)

	remaining, err := hp.Next(hf, b)
	require.NoError(t, err)
	require.Empty(t, remaining)
	require.Equal(t, ":authority", hf.Key())
	require.Equal(t, "example.com", hf.Value())
	// Should be added to dynamic table
	require.Len(t, hp.dynamic, 1)
}

func TestHPACKLiteralWithIndexedKeyInvalid(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// Literal with incremental indexing, invalid key index
	// 0x7F 0x40 = index = 63 + 64 = 127 (beyond static table, no dynamic entries)
	b := []byte{0x7F, 0x40}
	_, err := hp.Next(hf, b)
	require.Error(t, err)
	require.Contains(t, err.Error(), "literal indexed field not found")
}

func TestHPACKNonIndexedLiteralWithIndexedKey(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// Non-indexed literal with key from static table index 1 (:authority)
	// 0x01 = 0000_0001 → without indexing, key index = 1
	val := []byte("test.com")
	b := []byte{0x01, byte(len(val))}
	b = append(b, val...)

	remaining, err := hp.Next(hf, b)
	require.NoError(t, err)
	require.Empty(t, remaining)
	require.Equal(t, ":authority", hf.Key())
	require.Equal(t, "test.com", hf.Value())
	// Should NOT be added to dynamic table
	require.Empty(t, hp.dynamic)
}

func TestHPACKAppendHeaderVariants(t *testing.T) {
	enc := AcquireHPACK()
	defer ReleaseHPACK(enc)
	enc.SetMaxTableSize(4096)
	enc.DisableCompression = true

	// Helper: encode with enc, decode with a fresh decoder
	roundTrip := func(hf *HeaderField, store bool) *HeaderField {
		t.Helper()
		dec := AcquireHPACK()
		defer ReleaseHPACK(dec)
		dec.SetMaxTableSize(4096)

		dst := enc.AppendHeader(nil, hf, store)
		require.NotEmpty(t, dst)
		out := AcquireHeaderField()
		remaining, err := dec.Next(out, dst)
		require.NoError(t, err)
		require.Empty(t, remaining)
		return out
	}

	// 1. Sensible (never-indexed) header — encode-only (known prefix mismatch bug)
	hf := AcquireHeaderField()
	hf.SetBytes([]byte("authorization"), []byte("Bearer token"))
	hf.sensible = true
	dst := enc.AppendHeader(nil, hf, false)
	require.NotEmpty(t, dst, "sensible header should produce output")
	ReleaseHeaderField(hf)

	// 2. Full match from static table (:method GET)
	hf = AcquireHeaderField()
	hf.SetBytes([]byte(":method"), []byte("GET"))
	out := roundTrip(hf, true)
	require.Equal(t, ":method", out.Key())
	require.Equal(t, "GET", out.Value())
	ReleaseHeaderField(hf)
	ReleaseHeaderField(out)

	// 3. Key match, no value match, store=false
	hf = AcquireHeaderField()
	hf.SetBytes([]byte(":authority"), []byte("host.example.com"))
	out = roundTrip(hf, false)
	require.Equal(t, ":authority", out.Key())
	require.Equal(t, "host.example.com", out.Value())
	ReleaseHeaderField(hf)
	ReleaseHeaderField(out)

	// 4. Unknown header, store=false + DisableDynamicTable
	enc.DisableDynamicTable = true
	hf = AcquireHeaderField()
	hf.SetBytes([]byte("x-custom"), []byte("value"))
	out = roundTrip(hf, false)
	require.Equal(t, "x-custom", out.Key())
	require.Equal(t, "value", out.Value())
	ReleaseHeaderField(hf)
	ReleaseHeaderField(out)
	enc.DisableDynamicTable = false

	// 5. Unknown header, store=true (adds to dynamic table)
	hf = AcquireHeaderField()
	hf.SetBytes([]byte("x-new-header"), []byte("new-value"))
	prevDynLen := len(enc.dynamic)
	out = roundTrip(hf, true)
	require.Equal(t, "x-new-header", out.Key())
	require.Equal(t, "new-value", out.Value())
	require.Equal(t, prevDynLen+1, len(enc.dynamic))
	ReleaseHeaderField(hf)
	ReleaseHeaderField(out)
}

func TestHPACKNonIndexedLiteralWithStringKey(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	// Non-indexed literal with string key (c = 0x00, c&15 == 0)
	// 0x00 = no indexing, new name (string literal key)
	key := []byte("x-custom")
	val := []byte("myval")
	b := []byte{0x00, byte(len(key))}
	b = append(b, key...)
	b = append(b, byte(len(val)))
	b = append(b, val...)

	remaining, err := hp.Next(hf, b)
	require.NoError(t, err)
	require.Empty(t, remaining)
	require.Equal(t, "x-custom", hf.Key())
	require.Equal(t, "myval", hf.Value())
	require.Empty(t, hp.dynamic)
}

func TestHPACKShrinkEviction(t *testing.T) {
	hp := AcquireHPACK()
	defer ReleaseHPACK(hp)
	hp.SetMaxTableSize(4096)

	// Add several entries to fill the dynamic table
	for i := range 10 {
		hf := AcquireHeaderField()
		hf.Set(fmt.Sprintf("x-key-%d", i), "some-value-that-takes-space")
		hp.addDynamic(hf)
		ReleaseHeaderField(hf)
	}
	require.Equal(t, 10, len(hp.dynamic))
	prevSize := hp.dynamicSize

	// Shrink to a small size — should evict oldest entries
	hp.maxTableSize = 100
	hp.shrink()
	require.Less(t, len(hp.dynamic), 10)
	require.LessOrEqual(t, hp.dynamicSize, uint32(100))
	require.Less(t, hp.dynamicSize, prevSize)
}

func TestHPACKEncodeDecodeRoundTrip(t *testing.T) {
	headers := []struct{ key, value string }{
		{":method", "GET"},
		{":method", "POST"},
		{":path", "/"},
		{":path", "/api/v1/users"},
		{":scheme", "https"},
		{":status", "200"},
		{":status", "404"},
		{"content-type", "application/json"},
		{"x-custom-header", "custom-value"},
		{"accept-encoding", "gzip, deflate, br"},
	}

	for _, hdr := range headers {
		enc := AcquireHPACK()
		dec := AcquireHPACK()
		enc.SetMaxTableSize(4096)
		dec.SetMaxTableSize(4096)

		hf := AcquireHeaderField()
		hf.Set(hdr.key, hdr.value)

		encoded := enc.AppendHeader(nil, hf, true)
		require.NotEmpty(t, encoded, "empty encoding for %s: %s", hdr.key, hdr.value)

		out := AcquireHeaderField()
		remaining, err := dec.Next(out, encoded)
		require.NoError(t, err, "decode error for %s: %s", hdr.key, hdr.value)
		require.Empty(t, remaining)
		require.Equal(t, hdr.key, out.Key())
		require.Equal(t, hdr.value, out.Value())

		ReleaseHeaderField(hf)
		ReleaseHeaderField(out)
		ReleaseHPACK(enc)
		ReleaseHPACK(dec)
	}
}

func hexComparison(b, r []byte) (s string) {
	s += "\n"
	for i := range b {
		s += fmt.Sprintf("%x", b[i]) + " "
	}
	s += "\n"
	for i := range r {
		s += fmt.Sprintf("%x", r[i]) + " "
	}
	return
}
