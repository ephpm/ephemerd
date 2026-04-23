package dind

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestStreamMux_FrameLayout(t *testing.T) {
	var buf bytes.Buffer
	mux := newStreamMux(&buf)
	out := &streamMuxWriter{mux: mux, stream: stdcopyStdout}
	errW := &streamMuxWriter{mux: mux, stream: stdcopyStderr}

	if _, err := out.Write([]byte("hello")); err != nil {
		t.Fatalf("stdout write: %v", err)
	}
	if _, err := errW.Write([]byte("oops!")); err != nil {
		t.Fatalf("stderr write: %v", err)
	}

	got := buf.Bytes()
	// Expected: [01 00 00 00 00 00 00 05] "hello" [02 00 00 00 00 00 00 05] "oops!"
	expect := []byte{
		stdcopyStdout, 0, 0, 0, 0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o',
		stdcopyStderr, 0, 0, 0, 0, 0, 0, 5, 'o', 'o', 'p', 's', '!',
	}
	if !bytes.Equal(got, expect) {
		t.Errorf("frames mismatch\n got:  %s\n want: %s", hex.EncodeToString(got), hex.EncodeToString(expect))
	}
}

func TestStreamMux_EmptyWriteIsNoop(t *testing.T) {
	var buf bytes.Buffer
	mux := newStreamMux(&buf)
	out := &streamMuxWriter{mux: mux, stream: stdcopyStdout}
	n, err := out.Write(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("n=%d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Errorf("buf len=%d, want 0 (empty writes must not emit a frame header)", buf.Len())
	}
}

func TestStreamMux_LargePayloadHeader(t *testing.T) {
	var buf bytes.Buffer
	mux := newStreamMux(&buf)
	out := &streamMuxWriter{mux: mux, stream: stdcopyStdout}
	payload := bytes.Repeat([]byte{'x'}, 70000)
	if _, err := out.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := buf.Bytes()
	if len(got) != 8+len(payload) {
		t.Fatalf("buf size = %d, want %d", len(got), 8+len(payload))
	}
	size := binary.BigEndian.Uint32(got[4:8])
	if int(size) != len(payload) {
		t.Errorf("frame size header = %d, want %d", size, len(payload))
	}
}

func TestStreamMux_ConcurrentWritesDoNotInterleaveFrames(t *testing.T) {
	// If the mutex is missing, frame headers and payloads from different
	// goroutines will interleave and a stdcopy parser will desync. We
	// detect this by parsing the output and checking that every payload
	// length matches the advertised length.
	var buf bytes.Buffer
	mux := newStreamMux(&buf)
	out := &streamMuxWriter{mux: mux, stream: stdcopyStdout}
	errW := &streamMuxWriter{mux: mux, stream: stdcopyStderr}

	const iters = 500
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iters {
			if _, err := out.Write([]byte("AAAAA")); err != nil {
				t.Errorf("out write: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range iters {
			if _, err := errW.Write([]byte("BB")); err != nil {
				t.Errorf("err write: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	data := buf.Bytes()
	offset := 0
	outCount, errCount := 0, 0
	for offset < len(data) {
		if len(data)-offset < 8 {
			t.Fatalf("truncated frame header at offset %d", offset)
		}
		stream := data[offset]
		size := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if offset+size > len(data) {
			t.Fatalf("frame at offset %d claims size %d but only %d bytes remain", offset-8, size, len(data)-offset)
		}
		payload := data[offset : offset+size]
		offset += size
		switch stream {
		case stdcopyStdout:
			if !bytes.Equal(payload, []byte("AAAAA")) {
				t.Fatalf("stdout frame payload = %q, want AAAAA", payload)
			}
			outCount++
		case stdcopyStderr:
			if !bytes.Equal(payload, []byte("BB")) {
				t.Fatalf("stderr frame payload = %q, want BB", payload)
			}
			errCount++
		default:
			t.Fatalf("unknown stream byte: %d", stream)
		}
	}
	if outCount != iters || errCount != iters {
		t.Errorf("counts: out=%d err=%d, want %d each", outCount, errCount, iters)
	}
}

func TestWantsHijack(t *testing.T) {
	cases := []struct {
		name        string
		upgrade     string
		connection  string
		want        bool
	}{
		{"docker client canonical", "tcp", "Upgrade", true},
		{"case-insensitive upgrade", "TCP", "upgrade", true},
		{"multi-token connection header", "tcp", "keep-alive, Upgrade", true},
		{"missing upgrade header", "", "Upgrade", false},
		{"missing connection token", "tcp", "keep-alive", false},
		{"wrong upgrade protocol", "websocket", "Upgrade", false},
		{"both empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/exec/abc/start", nil)
			if tc.upgrade != "" {
				r.Header.Set("Upgrade", tc.upgrade)
			}
			if tc.connection != "" {
				r.Header.Set("Connection", tc.connection)
			}
			if got := wantsHijack(r); got != tc.want {
				t.Errorf("wantsHijack = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHijackConn_Handshake drives a real HTTP server so http.ResponseWriter
// actually satisfies http.Hijacker. We speak the protocol by hand from the
// client so we can inspect the 101 status line byte-for-byte.
func TestHijackConn_Handshake(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		conn, _, err := hijackConn(w, contentTypeMuxStream)
		if err != nil {
			t.Errorf("hijackConn: %v", err)
			return
		}
		defer conn.Close()
		// Send a single stdout frame so the test can verify end-to-end wiring.
		mux := newStreamMux(conn)
		if _, err := (&streamMuxWriter{mux: mux, stream: stdcopyStdout}).Write([]byte("hi")); err != nil {
			t.Errorf("mux write: %v", err)
		}
	}))
	defer server.Close()

	addr := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := "POST /exec/abc/start HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: tcp\r\n" +
		"Connection: Upgrade\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status line: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 101") {
		t.Errorf("status line = %q, want 101", statusLine)
	}
	// Read headers until the empty line
	seenContentType := ""
	seenUpgrade := ""
	seenConnection := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading header: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(name) {
		case "content-type":
			seenContentType = val
		case "upgrade":
			seenUpgrade = val
		case "connection":
			seenConnection = val
		}
	}
	if seenContentType != contentTypeMuxStream {
		t.Errorf("Content-Type = %q, want %q", seenContentType, contentTypeMuxStream)
	}
	if !strings.EqualFold(seenUpgrade, "tcp") {
		t.Errorf("Upgrade = %q, want tcp", seenUpgrade)
	}
	if !strings.EqualFold(seenConnection, "Upgrade") {
		t.Errorf("Connection = %q, want Upgrade", seenConnection)
	}

	// Now the body is a multiplexed-stream frame: stdout "hi"
	header := make([]byte, 8)
	if _, err := io.ReadFull(br, header); err != nil {
		t.Fatalf("reading frame header: %v", err)
	}
	if header[0] != stdcopyStdout {
		t.Errorf("frame stream byte = %d, want stdout (%d)", header[0], stdcopyStdout)
	}
	size := binary.BigEndian.Uint32(header[4:])
	if size != 2 {
		t.Errorf("frame size = %d, want 2", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatalf("reading frame payload: %v", err)
	}
	if string(payload) != "hi" {
		t.Errorf("payload = %q, want hi", payload)
	}

	<-done
}
