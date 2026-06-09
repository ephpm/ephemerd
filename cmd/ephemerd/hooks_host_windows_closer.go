//go:build windows

package main

import (
	"log/slog"
	"sync"
)

// hcsCloserSet tracks per-container HCS Close functions handed back by
// metrics.NewWindowsSamplerByID. We need to Close the HCS handle on
// container destroy or the host leaks compute-system references; the
// runtime hook doesn't carry that closer through to onDestroy, so we
// keep a side map.
type hcsCloserSet struct {
	mu      sync.Mutex
	closers map[string]func() error
}

func newHCSCloserSet() *hcsCloserSet {
	return &hcsCloserSet{closers: make(map[string]func() error)}
}

func (s *hcsCloserSet) add(id string, c func() error) {
	if c == nil {
		return
	}
	s.mu.Lock()
	s.closers[id] = c
	s.mu.Unlock()
}

func (s *hcsCloserSet) closeAndRemove(id string) {
	s.mu.Lock()
	c, ok := s.closers[id]
	delete(s.closers, id)
	s.mu.Unlock()
	if !ok || c == nil {
		return
	}
	if err := c(); err != nil {
		slog.Default().Debug("hcs sampler close failed", "id", id, "error", err)
	}
}
