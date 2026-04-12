package vm

import "testing"

func TestDecodeWSLOutput_UTF16LE_WithBOM(t *testing.T) {
	// UTF-16LE BOM (FF FE) + "hi" in UTF-16LE
	input := []byte{0xFF, 0xFE, 'h', 0x00, 'i', 0x00}
	got := decodeWSLOutput(input)
	if got != "hi" {
		t.Errorf("decodeWSLOutput(BOM+hi) = %q, want %q", got, "hi")
	}
}

func TestDecodeWSLOutput_UTF16LE_NoBOM(t *testing.T) {
	// "hello" in UTF-16LE without BOM
	input := []byte{'h', 0x00, 'e', 0x00, 'l', 0x00, 'l', 0x00, 'o', 0x00}
	got := decodeWSLOutput(input)
	if got != "hello" {
		t.Errorf("decodeWSLOutput(hello UTF-16LE) = %q, want %q", got, "hello")
	}
}

func TestDecodeWSLOutput_UTF8_Passthrough(t *testing.T) {
	// Plain UTF-8 string (no null bytes in odd positions)
	input := []byte("plain utf-8 text")
	got := decodeWSLOutput(input)
	if got != "plain utf-8 text" {
		t.Errorf("decodeWSLOutput(utf8) = %q, want %q", got, "plain utf-8 text")
	}
}

func TestDecodeWSLOutput_OddLength(t *testing.T) {
	// Odd-length byte slice can't be UTF-16, returned as-is
	input := []byte{0xFF, 0xFE, 'a'}
	got := decodeWSLOutput(input)
	// After stripping BOM, we have 1 byte — odd length, returned as UTF-8
	if got != "a" {
		t.Errorf("decodeWSLOutput(odd) = %q, want %q", got, "a")
	}
}

func TestDecodeWSLOutput_Empty(t *testing.T) {
	got := decodeWSLOutput(nil)
	if got != "" {
		t.Errorf("decodeWSLOutput(nil) = %q, want empty", got)
	}

	got = decodeWSLOutput([]byte{})
	if got != "" {
		t.Errorf("decodeWSLOutput(empty) = %q, want empty", got)
	}
}

func TestDecodeWSLOutput_BOMOnly(t *testing.T) {
	input := []byte{0xFF, 0xFE}
	got := decodeWSLOutput(input)
	if got != "" {
		t.Errorf("decodeWSLOutput(BOM only) = %q, want empty", got)
	}
}

func TestDecodeWSLOutput_UTF16LE_MultipleLines(t *testing.T) {
	// "a\nb" in UTF-16LE
	input := []byte{'a', 0x00, '\n', 0x00, 'b', 0x00}
	got := decodeWSLOutput(input)
	if got != "a\nb" {
		t.Errorf("decodeWSLOutput(multiline) = %q, want %q", got, "a\nb")
	}
}
