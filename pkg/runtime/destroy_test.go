package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// TestWaitTaskExit pins the bounded kill-wait that replaced a bare
// <-exitCh in Destroy. The production failure mode is a dead containerd
// shim: the killed task's exit event never arrives, and the unbounded
// receive blocked Destroy forever, pinning a scheduler concurrency slot
// for over an hour. The wait must give up on its own — even under
// context.Background(), which is what every Destroy caller passes.
func TestWaitTaskExit(t *testing.T) {
	t.Run("exit event arrives", func(t *testing.T) {
		exitCh := make(chan client.ExitStatus, 1)
		exitCh <- client.ExitStatus{}
		if !waitTaskExit(context.Background(), exitCh, time.Minute) {
			t.Fatal("waitTaskExit = false, want true when the exit event is delivered")
		}
	})

	t.Run("dead shim never delivers: killWait bounds the wait", func(t *testing.T) {
		exitCh := make(chan client.ExitStatus) // never written — the dead-shim case
		start := time.Now()
		// Background ctx on purpose: the bound must hold with no caller deadline.
		if waitTaskExit(context.Background(), exitCh, 20*time.Millisecond) {
			t.Fatal("waitTaskExit = true, want false when no exit event ever arrives")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("waitTaskExit took %v, want ~20ms — the bound did not hold", elapsed)
		}
	})

	t.Run("ctx expiry bounds the wait before killWait", func(t *testing.T) {
		exitCh := make(chan client.ExitStatus)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if waitTaskExit(ctx, exitCh, time.Hour) {
			t.Fatal("waitTaskExit = true, want false on cancelled ctx")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("waitTaskExit took %v, want immediate return on cancelled ctx", elapsed)
		}
	})
}

// TestDestroyWaitCompleted pins the caller-side wait: bounded, and when a
// teardown finishes right at the deadline the completed result must win —
// a completed teardown mis-logged as "abandoned" sends an operator hunting
// for a leak that does not exist.
func TestDestroyWaitCompleted(t *testing.T) {
	t.Run("teardown finishes first", func(t *testing.T) {
		done := make(chan struct{}, 1)
		done <- struct{}{}
		if !destroyWaitCompleted(context.Background(), done) {
			t.Fatal("destroyWaitCompleted = false, want true when teardown already finished")
		}
	})

	t.Run("deadline expires with teardown still wedged", func(t *testing.T) {
		done := make(chan struct{}, 1) // never written — the wedged case
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		if destroyWaitCompleted(ctx, done) {
			t.Fatal("destroyWaitCompleted = true, want false when teardown never finishes")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("destroyWaitCompleted took %v, want ~20ms", elapsed)
		}
	})

	t.Run("both ready: done wins over the expired ctx", func(t *testing.T) {
		done := make(chan struct{}, 1)
		done <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // ctx already expired AND done already delivered
		// The select picks randomly between two ready cases, so run it
		// enough times that a regression to coin-flip behavior cannot pass.
		for i := 0; i < 100; i++ {
			if !destroyWaitCompleted(ctx, done) {
				t.Fatal("destroyWaitCompleted = false with done ready, want the completed teardown to win")
			}
			done <- struct{}{} // refill for the next iteration
		}
	})
}
