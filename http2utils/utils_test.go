package http2utils

import (
	"bytes"
	"testing"
)

func TestUintConversions(t *testing.T) {
	b := make([]byte, 3)
	Uint24ToBytes(b, 0x010203)
	if got := BytesToUint24(b); got != 0x010203 {
		t.Fatalf("unexpected uint24: %x", got)
	}

	b4 := make([]byte, 4)
	Uint32ToBytes(b4, 0x11223344)
	if got := BytesToUint32(b4); got != 0x11223344 {
		t.Fatalf("unexpected uint32: %x", got)
	}
}

func TestEqualsFoldAndResize(t *testing.T) {
	if !EqualsFold([]byte("GoLang"), []byte("golang")) {
		t.Fatalf("expected equals fold")
	}
	if EqualsFold([]byte("Go"), []byte("lang")) {
		t.Fatalf("unexpected equals fold match")
	}

	resized := Resize(make([]byte, 0, 1), 4)
	if len(resized) != 4 {
		t.Fatalf("resize failed: %d", len(resized))
	}
}

func TestPaddingHelpers(t *testing.T) {
	src := []byte("data")
	padded := AddPadding(src)
	if len(padded) <= len(src)+1 {
		t.Fatalf("expected extra padding bytes")
	}

	trimmed, err := CutPadding(padded, len(padded))
	if err != nil {
		t.Fatalf("cut padding: %v", err)
	}
	if !bytes.Equal(trimmed, src) {
		t.Fatalf("unexpected trimmed payload: %q", trimmed)
	}
}

func TestFastBytesToString(t *testing.T) {
	b := []byte("hello")
	if FastBytesToString(b) != "hello" {
		t.Fatalf("unexpected string conversion")
	}
}
