package dind

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Docker's multiplexed stream framing uses an 8-byte header per chunk:
//
//	[stream, 0, 0, 0, size_be_uint32]
//
// The 3 zero bytes are padding. `stream` is one of stdcopyStd{in,out,err}.
// Clients parse this via github.com/docker/docker/pkg/stdcopy.StdCopy.
const (
	stdcopyStdin  byte = 0
	stdcopyStdout byte = 1
	stdcopyStderr byte = 2

	// Docker content types for the 101 Upgrade response. Older clients
	// accept raw-stream; newer ones detect multiplexed-stream and invoke
	// their stdcopy demuxer. Reporting the multiplexed type is the
	// compatible choice for non-TTY exec/attach.
	contentTypeRawStream   = "application/vnd.docker.raw-stream"
	contentTypeMuxStream   = "application/vnd.docker.multiplexed-stream"
)

// streamMux serializes concurrent stdout/stderr writes onto a single
// hijacked connection so frame headers never interleave with payload from
// another stream.
type streamMux struct {
	mu sync.Mutex
	w  io.Writer
}

func newStreamMux(w io.Writer) *streamMux {
	return &streamMux{w: w}
}

// write emits one framed chunk of the given stream kind.
func (m *streamMux) write(stream byte, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var header [8]byte
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(p)))
	if _, err := m.w.Write(header[:]); err != nil {
		return 0, err
	}
	if _, err := m.w.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// streamMuxWriter is an io.Writer bound to one stream kind.
type streamMuxWriter struct {
	mux    *streamMux
	stream byte
}

func (w *streamMuxWriter) Write(p []byte) (int, error) {
	return w.mux.write(w.stream, p)
}

// rawStreamWriter is a drop-in for TTY mode where stdout+stderr are
// interleaved without framing (the single-stream case).
type rawStreamWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *rawStreamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// wantsHijack reports whether the client requested an HTTP upgrade to raw
// TCP (the Docker Engine hijack handshake). Both "tcp" and the literal
// "Upgrade" token must be present; we accept either case.
func wantsHijack(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
		return false
	}
	for _, tok := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "Upgrade") {
			return true
		}
	}
	return false
}

// hijackConn takes over the underlying TCP connection and writes Docker's
// 101 UPGRADED status line. Returns the hijacked conn + buffered reader
// (which may hold pipelined bytes read before hijack) for caller to wire to
// stdin, plus a cleanup that closes the conn.
//
// contentType chooses between raw and multiplexed; pass contentTypeMuxStream
// for non-TTY exec and contentTypeRawStream for TTY exec or attach with tty.
func hijackConn(w http.ResponseWriter, contentType string) (net.Conn, *bufio.Reader, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("hijacking connection: %w", err)
	}
	// Docker writes the status line directly on the hijacked conn rather
	// than through ResponseWriter — ResponseWriter is no longer safe to
	// use once Hijack() succeeds.
	resp := "HTTP/1.1 101 UPGRADED\r\n" +
		"Content-Type: " + contentType + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("writing 101 response: %w", err)
	}
	return conn, buf.Reader, nil
}
