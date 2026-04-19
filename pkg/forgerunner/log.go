package forgerunner

import (
	"context"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
)

// LogReporter batches log lines and streams them to the forge via UpdateLog.
type LogReporter struct {
	client *forgerpc.Client
	taskID int64
	masker *SecretMasker

	mu       sync.Mutex
	rows     []forgerpc.LogRow
	sent     int64 // total rows sent (acked by server)
	total    int64 // total rows added
	ackIndex int64
}

// NewLogReporter creates a log reporter for the given task.
func NewLogReporter(client *forgerpc.Client, taskID int64, masker *SecretMasker) *LogReporter {
	return &LogReporter{
		client: client,
		taskID: taskID,
		masker: masker,
	}
}

// AddLine appends a timestamped, secret-masked log line.
func (r *LogReporter) AddLine(content string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.masker != nil {
		content = r.masker.Mask(content)
	}
	r.rows = append(r.rows, forgerpc.LogRow{
		Time:    time.Now(),
		Content: content,
	})
	r.total++
}

// LineCount returns the total number of log lines added.
func (r *LogReporter) LineCount() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// Flush sends any buffered log lines to the forge.
func (r *LogReporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	rows := r.rows
	r.rows = nil
	index := r.ackIndex
	r.mu.Unlock()

	if len(rows) == 0 {
		return nil
	}

	ack, err := r.client.UpdateLog(ctx, r.taskID, index, rows, false)
	if err != nil {
		// Re-queue the rows for retry.
		r.mu.Lock()
		r.rows = append(rows, r.rows...)
		r.mu.Unlock()
		return err
	}

	r.mu.Lock()
	r.ackIndex = ack
	r.sent += int64(len(rows))
	r.mu.Unlock()
	return nil
}

// Close flushes remaining lines and sends the final noMore=true signal.
func (r *LogReporter) Close(ctx context.Context) error {
	r.mu.Lock()
	rows := r.rows
	r.rows = nil
	index := r.ackIndex
	r.mu.Unlock()

	_, err := r.client.UpdateLog(ctx, r.taskID, index, rows, true)
	return err
}
