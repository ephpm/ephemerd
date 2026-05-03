package localtunnel

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCounter_AddAccumulates(t *testing.T) {
	var c counter
	c.Add(3)
	c.Add(4)

	c.m.Lock()
	got := c.counter
	c.m.Unlock()

	if got != 7 {
		t.Errorf("counter = %d, want 7", got)
	}
}

func TestCounter_AddNegative(t *testing.T) {
	var c counter
	c.Add(10)
	c.Add(-3)

	c.m.Lock()
	got := c.counter
	c.m.Unlock()

	if got != 7 {
		t.Errorf("counter = %d, want 7", got)
	}
}

func TestCounter_WaitForReturnsImmediatelyIfTargetReached(t *testing.T) {
	var c counter
	c.Add(5)

	done := make(chan struct{})
	go func() {
		c.WaitFor(5)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor blocked even though counter >= target")
	}
}

func TestCounter_WaitForReturnsImmediatelyIfTargetIsZero(t *testing.T) {
	var c counter

	done := make(chan struct{})
	go func() {
		c.WaitFor(0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor(0) should not block on zero counter")
	}
}

func TestCounter_WaitForUnblocksOnAdd(t *testing.T) {
	var c counter

	var unblocked atomic.Bool
	done := make(chan struct{})
	go func() {
		c.WaitFor(3)
		unblocked.Store(true)
		close(done)
	}()

	// Give the waiter a moment to enter Wait().
	time.Sleep(50 * time.Millisecond)
	if unblocked.Load() {
		t.Fatal("waiter unblocked before counter reached target")
	}

	c.Add(2)
	time.Sleep(50 * time.Millisecond)
	if unblocked.Load() {
		t.Fatal("waiter unblocked at counter=2, target was 3")
	}

	c.Add(1) // counter now 3
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not unblock when counter reached target")
	}
}

func TestCounter_WaitForOvershoot(t *testing.T) {
	var c counter

	done := make(chan struct{})
	go func() {
		c.WaitFor(5)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	c.Add(100) // overshoot

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitFor did not unblock on overshoot")
	}
}

func TestCounter_MultipleWaitersAllUnblock(t *testing.T) {
	var c counter
	const n = 5

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			c.WaitFor(1)
		}()
	}

	time.Sleep(50 * time.Millisecond)
	c.Add(1)

	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast did not wake all waiters")
	}
}
