package forgerunner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
)

// fakeUpdateLogServer mocks the runner.v1.RunnerService UpdateLog endpoint
// and lets tests assert the request payload and control the response.
func fakeUpdateLogServer(t *testing.T, ackResponse int64, fail bool, capture *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/actions/runner.v1.RunnerService/UpdateLog", func(w http.ResponseWriter, r *http.Request) {
		capture.Add(1)
		if fail {
			http.Error(w, `{"code":"internal","message":"boom"}`, http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Logf("drain body: %v", err)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"ackIndex": ackResponse,
		}); err != nil {
			t.Logf("encode response: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLogReporter_AddLineIncrementsTotal(t *testing.T) {
	rep := NewLogReporter(nil, 0, nil)

	rep.AddLine("hello")
	rep.AddLine("world")

	if got := rep.LineCount(); got != 2 {
		t.Errorf("LineCount = %d, want 2", got)
	}
}

func TestLogReporter_AddLineMasking(t *testing.T) {
	masker := NewSecretMasker([]string{"topsecret"})
	rep := NewLogReporter(nil, 0, masker)

	rep.AddLine("user said: topsecret")

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if got := rep.rows[0].Content; strings.Contains(got, "topsecret") {
		t.Errorf("masker did not redact: %q", got)
	}
}

func TestLogReporter_FlushNoRows_NoOp(t *testing.T) {
	rep := NewLogReporter(nil, 0, nil)
	if err := rep.Flush(context.Background()); err != nil {
		t.Errorf("Flush with no rows = %v, want nil", err)
	}
}

func TestLogReporter_FlushSendsBatchedRows(t *testing.T) {
	var calls atomic.Int32
	srv := fakeUpdateLogServer(t, 5, false, &calls)

	c := forgerpc.NewClient(srv.URL, nil)
	rep := NewLogReporter(c, 99, nil)

	rep.AddLine("line1")
	rep.AddLine("line2")
	rep.AddLine("line3")

	if err := rep.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d, want 1", got)
	}

	// After flush, buffer is drained and ackIndex advanced.
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.rows) != 0 {
		t.Errorf("buffer should be drained, got %d rows", len(rep.rows))
	}
	if rep.ackIndex != 5 {
		t.Errorf("ackIndex = %d, want 5", rep.ackIndex)
	}
	if rep.sent != 3 {
		t.Errorf("sent = %d, want 3", rep.sent)
	}
}

func TestLogReporter_FlushOnFailureRequeuesRows(t *testing.T) {
	var calls atomic.Int32
	srv := fakeUpdateLogServer(t, 0, true, &calls)

	c := forgerpc.NewClient(srv.URL, nil)
	rep := NewLogReporter(c, 99, nil)

	rep.AddLine("a")
	rep.AddLine("b")

	err := rep.Flush(context.Background())
	if err == nil {
		t.Fatal("Flush should return error on server failure")
	}

	// Rows should be re-queued for retry.
	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.rows) != 2 {
		t.Errorf("re-queued rows = %d, want 2", len(rep.rows))
	}
	if rep.ackIndex != 0 {
		t.Errorf("ackIndex should not advance on failure: got %d", rep.ackIndex)
	}
	if rep.sent != 0 {
		t.Errorf("sent should not advance on failure: got %d", rep.sent)
	}
}

func TestLogReporter_CloseSendsNoMore(t *testing.T) {
	var calls atomic.Int32

	mux := http.NewServeMux()
	var sawNoMore atomic.Bool
	mux.HandleFunc("/api/actions/runner.v1.RunnerService/UpdateLog", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		if no, _ := body["noMore"].(bool); no {
			sawNoMore.Store(true)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"ackIndex": 1}); err != nil {
			t.Logf("encode: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := forgerpc.NewClient(srv.URL, nil)
	rep := NewLogReporter(c, 99, nil)

	rep.AddLine("only line")
	if err := rep.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !sawNoMore.Load() {
		t.Error("Close should send noMore=true")
	}
	if calls.Load() == 0 {
		t.Error("Close should make at least one server call")
	}
}

func TestLogReporter_CloseWithEmptyBuffer(t *testing.T) {
	var calls atomic.Int32
	srv := fakeUpdateLogServer(t, 0, false, &calls)

	c := forgerpc.NewClient(srv.URL, nil)
	rep := NewLogReporter(c, 99, nil)

	// No AddLine calls — Close should still send noMore=true so the
	// server knows to finalize the log.
	if err := rep.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 server call (noMore signal), got %d", calls.Load())
	}
}

func TestLogReporter_LineCountThreadSafe(t *testing.T) {
	rep := NewLogReporter(nil, 0, nil)
	const n = 100
	done := make(chan struct{}, 4)
	for w := 0; w < 4; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < n; i++ {
				rep.AddLine("x")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if got := rep.LineCount(); got != 4*n {
		t.Errorf("LineCount = %d, want %d", got, 4*n)
	}
}
