package protocol

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodeLockSerializesSameAccount(t *testing.T) {
	p := NewPool(DefaultConfig(), nil)
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var wg sync.WaitGroup

	acquireForTest := func(entered chan<- struct{}, accountID int64, releaseSignal <-chan struct{}) {
		defer wg.Done()
		// 本项目用 acquireScoped 以账号 ID 为键取取码锁（等价于上游的 codeLockFor）
		unlock, err := acquireScoped(context.Background(), &p.mu, p.codeLocks, accountID, func() *keyLock {
			return &keyLock{ch: make(chan struct{}, 1)}
		})
		if err != nil {
			t.Errorf("acquire code lock: %v", err)
			return
		}
		defer unlock()
		close(entered)
		<-releaseSignal
	}

	wg.Add(2)
	go acquireForTest(firstEntered, 7, releaseFirst)
	<-firstEntered
	go acquireForTest(secondEntered, 7, releaseSecond)

	select {
	case <-secondEntered:
		t.Fatal("same account acquired two code locks concurrently")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second same-account request did not acquire after the first released")
	}
	close(releaseSecond)
	wg.Wait()
}

func TestCodeLocksAllowDifferentAccountsConcurrently(t *testing.T) {
	p := NewPool(DefaultConfig(), nil)
	started := make(chan struct{}, 2)
	releaseAll := make(chan struct{})
	var active int32
	var maxActive int32
	var wg sync.WaitGroup

	for _, accountID := range []int64{1, 2} {
		wg.Add(1)
		go func(accountID int64) {
			defer wg.Done()
			unlock, lockErr := acquireScoped(context.Background(), &p.mu, p.codeLocks, accountID, func() *keyLock {
				return &keyLock{ch: make(chan struct{}, 1)}
			})
			if lockErr != nil {
				t.Errorf("acquire code lock: %v", lockErr)
				return
			}
			defer unlock()
			current := atomic.AddInt32(&active, 1)
			for {
				old := atomic.LoadInt32(&maxActive)
				if current <= old || atomic.CompareAndSwapInt32(&maxActive, old, current) {
					break
				}
			}
			started <- struct{}{}
			<-releaseAll
			atomic.AddInt32(&active, -1)
		}(accountID)
	}

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different-account code locks did not run concurrently")
		}
	}
	if got := atomic.LoadInt32(&maxActive); got != 2 {
		t.Fatalf("max concurrent code locks = %d, want 2", got)
	}
	close(releaseAll)
	wg.Wait()
}
