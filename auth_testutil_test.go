package main

import (
	"context"
	"sync"
	"time"
)

// fakeGate 是 AuthGate 的测试替身：把 token 直接映射成 user_id，
// 在线登记只记在内存里。这样 /ws、/push 的 handler 测试不需要真的 MySQL / Redis。
type fakeGate struct {
	mu      sync.Mutex
	tokens  map[string]int64
	devices map[int64]map[string]string
}

func newFakeGate(tokens map[string]int64) *fakeGate {
	if tokens == nil {
		tokens = map[string]int64{}
	}
	return &fakeGate{tokens: tokens, devices: map[int64]map[string]string{}}
}

func (f *fakeGate) AuthenticateToken(_ context.Context, token string) (int64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.tokens[token]; ok {
		return id, "tester", nil
	}
	return 0, "", ErrInvalidCredentials
}

func (f *fakeGate) MarkOnline(_ context.Context, userID int64, deviceID, role string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.devices[userID] == nil {
		f.devices[userID] = map[string]string{}
	}
	f.devices[userID][deviceID] = role
	return nil
}

func (f *fakeGate) TouchOnline(context.Context, int64, time.Duration) error { return nil }

func (f *fakeGate) MarkOffline(_ context.Context, userID int64, deviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.devices[userID], deviceID)
	return nil
}

func (f *fakeGate) OnlineDevices(_ context.Context, userID int64) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.devices[userID] {
		out[k] = v
	}
	return out, nil
}

// withFakeGate 装上测试替身并返回还原函数，保证测试之间互不影响。
func withFakeGate(tokens map[string]int64) func() {
	orig := authGate
	authGate = newFakeGate(tokens)
	return func() { authGate = orig }
}
