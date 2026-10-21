package protocol

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newScopedTestLock 是 acquireScoped 的建锁工厂，与生产代码保持一致。
func newScopedTestLock() *keyLock {
	return &keyLock{ch: make(chan struct{}, 1)}
}

// TestScopedLocksAreReclaimed 验证锁表不会无限增长。
//
// 动态代理每次提取都会换出口，锁键里带着代理地址；如果条目只增不删，
// 长时间运行后 locks 表会变成只涨不落的内存泄漏。
func TestScopedLocksAreReclaimed(t *testing.T) {
	p := NewPool(DefaultConfig(), nil)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("account-%d\x00proxy-%d", i%5, i)
			unlock, err := acquireScoped(context.Background(), &p.mu, p.locks, key, newScopedTestLock)
			if err != nil {
				t.Errorf("acquireScoped(%s) error = %v", key, err)
				return
			}
			unlock()
		}(i)
	}
	wg.Wait()

	p.mu.Lock()
	remaining := len(p.locks)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("locks table leaked %d entries after all releases", remaining)
	}
}

// TestScopedLockSerializesSameKey 验证同一 key 的并发调用被真正串行化，
// 同时确认引用计数在并发下不会提前把表项删掉（否则会出现两把并行的锁）。
func TestScopedLockSerializesSameKey(t *testing.T) {
	p := NewPool(DefaultConfig(), nil)
	const key = "same-key"

	var inside int32
	var maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := acquireScoped(context.Background(), &p.mu, p.locks, key, newScopedTestLock)
			if err != nil {
				t.Errorf("acquireScoped() error = %v", err)
				return
			}
			defer unlock()
			current := atomic.AddInt32(&inside, 1)
			for {
				observed := atomic.LoadInt32(&maxInside)
				if current <= observed || atomic.CompareAndSwapInt32(&maxInside, observed, current) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
		}()
	}
	wg.Wait()

	if maxInside != 1 {
		t.Fatalf("max concurrent holders = %d, want 1 (lock did not serialize)", maxInside)
	}
	p.mu.Lock()
	remaining := len(p.locks)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("locks table leaked %d entries", remaining)
	}
}

// TestScopedLockCancelKeepsRefCountBalanced 验证排队阶段被取消时也会归还引用。
// 少还一次会让表项永久留在表里；多还一次会提前删掉仍被持有的锁。
func TestScopedLockCancelKeepsRefCountBalanced(t *testing.T) {
	p := NewPool(DefaultConfig(), nil)
	const key = "cancel-key"

	holderUnlock, err := acquireScoped(context.Background(), &p.mu, p.locks, key, newScopedTestLock)
	if err != nil {
		t.Fatalf("first acquireScoped() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireScoped(ctx, &p.mu, p.locks, key, newScopedTestLock); err == nil {
		t.Fatalf("acquireScoped() with cancelled ctx should fail")
	}

	// 锁仍被持有者占着，表项必须还在
	p.mu.Lock()
	entry := p.locks[key]
	p.mu.Unlock()
	if entry == nil {
		t.Fatalf("lock entry disappeared while still held")
	}

	holderUnlock()

	p.mu.Lock()
	remaining := len(p.locks)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("locks table leaked %d entries after cancellation", remaining)
	}
}
