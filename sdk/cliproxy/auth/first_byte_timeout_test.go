package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const ttfbTestModel = "ttfb-test-model"

// ttfbExecutor is a configurable openai-compatibility executor used by first-byte tests.
type ttfbExecutor struct {
	id       string
	execFn   func(ctx context.Context) (cliproxyexecutor.Response, error)
	streamFn func(ctx context.Context) (*cliproxyexecutor.StreamResult, error)

	mu    sync.Mutex
	calls int
}

func (e *ttfbExecutor) Identifier() string { return e.id }

func (e *ttfbExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return e.execFn(ctx)
}

func (e *ttfbExecutor) ExecuteStream(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return e.streamFn(ctx)
}

func (e *ttfbExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *ttfbExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *ttfbExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *ttfbExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// blockUntilCancelled simulates an upstream that never answers.
func blockUntilCancelled(ctx context.Context) (cliproxyexecutor.Response, error) {
	<-ctx.Done()
	return cliproxyexecutor.Response{}, ctx.Err()
}

func quickResponse(payload string) func(context.Context) (cliproxyexecutor.Response, error) {
	return func(context.Context) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{Payload: []byte(payload)}, nil
	}
}

// stallingStream returns headers immediately but never yields a data chunk.
func stallingStream(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(ch)
		<-ctx.Done()
		select {
		case ch <- cliproxyexecutor.StreamChunk{Err: ctx.Err()}:
		default:
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: ch}, nil
}

func chunkStream(chunks ...string) func(context.Context) (*cliproxyexecutor.StreamResult, error) {
	return func(context.Context) (*cliproxyexecutor.StreamResult, error) {
		ch := make(chan cliproxyexecutor.StreamChunk, len(chunks))
		for _, c := range chunks {
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte(c)}
		}
		close(ch)
		return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: ch}, nil
	}
}

type ttfbSetup struct {
	m          *Manager
	providers  []string
	slow, fast *ttfbExecutor
	slowAuthID string
}

// newTTFBSetup registers a high-priority "slow" compat provider and an optional low-priority
// "fast" compat provider serving the same model alias.
func newTTFBSetup(t *testing.T, timeoutSeconds *int, withFast bool, slow, fast *ttfbExecutor) *ttfbSetup {
	t.Helper()
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{FirstByteTimeoutSeconds: timeoutSeconds})
	reg := registry.GetGlobalRegistry()
	s := &ttfbSetup{m: m, slow: slow, fast: fast}

	add := func(name, priority string, exec *ttfbExecutor) {
		key := util.OpenAICompatibleProviderKey(name)
		exec.id = key
		id := "ttfb-" + name + "-" + t.Name()
		auth := &Auth{
			ID:         id,
			Provider:   name,
			Status:     StatusActive,
			Attributes: map[string]string{"priority": priority, "compat_name": name, "provider_key": name},
		}
		m.RegisterExecutor(exec)
		reg.RegisterClient(id, key, []*registry.ModelInfo{{ID: ttfbTestModel}})
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		t.Cleanup(func() { reg.UnregisterClient(id) })
		s.providers = append(s.providers, key)
		if exec == slow {
			s.slowAuthID = id
		}
	}
	add("ttfb-slow", "10", slow)
	if withFast {
		add("ttfb-fast", "0", fast)
	}
	return s
}

func intPtr(v int) *int { return &v }

func TestFirstByteTimeout_ConfigDefaults(t *testing.T) {
	if got := firstByteTimeoutFromConfig(nil); got != 25*time.Second {
		t.Fatalf("nil config: got %s, want 25s", got)
	}
	if got := firstByteTimeoutFromConfig(&internalconfig.Config{}); got != 25*time.Second {
		t.Fatalf("omitted: got %s, want 25s", got)
	}
	if got := firstByteTimeoutFromConfig(&internalconfig.Config{FirstByteTimeoutSeconds: intPtr(0)}); got != 0 {
		t.Fatalf("0: got %s, want disabled", got)
	}
	if got := firstByteTimeoutFromConfig(&internalconfig.Config{FirstByteTimeoutSeconds: intPtr(7)}); got != 7*time.Second {
		t.Fatalf("7: got %s, want 7s", got)
	}
}

func TestFirstByteTimeout_NonStreamFailsOverToNextPriority(t *testing.T) {
	slow := &ttfbExecutor{execFn: blockUntilCancelled}
	fast := &ttfbExecutor{execFn: quickResponse("fast")}
	s := newTTFBSetup(t, intPtr(1), true, slow, fast)

	start := time.Now()
	resp, err := s.m.Execute(context.Background(), s.providers, cliproxyexecutor.Request{Model: ttfbTestModel}, cliproxyexecutor.Options{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(resp.Payload) != "fast" {
		t.Fatalf("payload = %q, want fast", resp.Payload)
	}
	if slow.Calls() != 1 || fast.Calls() != 1 {
		t.Fatalf("calls slow=%d fast=%d, want 1/1", slow.Calls(), fast.Calls())
	}
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("elapsed %s, want ~1s", elapsed)
	}

	// The stalled upstream gets only the short first-byte cooldown.
	auth, ok := s.m.GetByID(s.slowAuthID)
	if !ok {
		t.Fatal("slow auth missing")
	}
	state := auth.ModelStates[canonicalModelKey(ttfbTestModel)]
	if state == nil || state.LastError == nil || state.LastError.Code != ErrorCodeFirstByteTimeout {
		t.Fatalf("expected first_byte_timeout model state, got %+v", state)
	}
	if wait := time.Until(state.NextRetryAfter); wait <= 0 || wait > firstByteTimeoutCooldown {
		t.Fatalf("cooldown %s, want (0, %s]", wait, firstByteTimeoutCooldown)
	}
}

func TestFirstByteTimeout_StreamFailsOverBeforeFirstChunk(t *testing.T) {
	slow := &ttfbExecutor{streamFn: stallingStream}
	fast := &ttfbExecutor{streamFn: chunkStream("a", "b")}
	s := newTTFBSetup(t, intPtr(1), true, slow, fast)

	start := time.Now()
	result, err := s.m.ExecuteStream(context.Background(), s.providers, cliproxyexecutor.Request{Model: ttfbTestModel}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var got string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		got += string(chunk.Payload)
	}
	elapsed := time.Since(start)
	if got != "ab" {
		t.Fatalf("stream = %q, want ab", got)
	}
	if slow.Calls() != 1 || fast.Calls() != 1 {
		t.Fatalf("calls slow=%d fast=%d, want 1/1", slow.Calls(), fast.Calls())
	}
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("elapsed %s, want ~1s", elapsed)
	}
}

// A stream whose first chunk is fast must never be cut, however long the rest takes.
func TestFirstByteTimeout_StreamNotKilledAfterFirstChunk(t *testing.T) {
	slow := &ttfbExecutor{streamFn: func(ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
		ch := make(chan cliproxyexecutor.StreamChunk)
		go func() {
			defer close(ch)
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-ctx.Done():
				ch <- cliproxyexecutor.StreamChunk{Err: ctx.Err()}
				return
			}
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte("-second")}
		}()
		return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: ch}, nil
	}}
	fast := &ttfbExecutor{streamFn: chunkStream("fast")}
	s := newTTFBSetup(t, intPtr(1), true, slow, fast)

	result, err := s.m.ExecuteStream(context.Background(), s.providers, cliproxyexecutor.Request{Model: ttfbTestModel}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var got string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		got += string(chunk.Payload)
	}
	if got != "first-second" {
		t.Fatalf("stream = %q, want first-second", got)
	}
	if fast.Calls() != 0 {
		t.Fatalf("fast provider called %d times, want 0", fast.Calls())
	}
}

func TestFirstByteTimeout_DisabledWithZero(t *testing.T) {
	slow := &ttfbExecutor{execFn: func(ctx context.Context) (cliproxyexecutor.Response, error) {
		select {
		case <-time.After(1500 * time.Millisecond):
			return cliproxyexecutor.Response{Payload: []byte("slow")}, nil
		case <-ctx.Done():
			return cliproxyexecutor.Response{}, ctx.Err()
		}
	}}
	fast := &ttfbExecutor{execFn: quickResponse("fast")}
	s := newTTFBSetup(t, intPtr(0), true, slow, fast)

	resp, err := s.m.Execute(context.Background(), s.providers, cliproxyexecutor.Request{Model: ttfbTestModel}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(resp.Payload) != "slow" || fast.Calls() != 0 {
		t.Fatalf("payload=%q fast calls=%d, want slow/0", resp.Payload, fast.Calls())
	}
}

// Without another candidate the guard is not armed: a slow-but-only upstream still answers.
func TestFirstByteTimeout_NotArmedWithoutAlternative(t *testing.T) {
	slow := &ttfbExecutor{execFn: func(ctx context.Context) (cliproxyexecutor.Response, error) {
		select {
		case <-time.After(1500 * time.Millisecond):
			return cliproxyexecutor.Response{Payload: []byte("slow")}, nil
		case <-ctx.Done():
			return cliproxyexecutor.Response{}, ctx.Err()
		}
	}}
	s := newTTFBSetup(t, intPtr(1), false, slow, nil)

	resp, err := s.m.Execute(context.Background(), s.providers, cliproxyexecutor.Request{Model: ttfbTestModel}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(resp.Payload) != "slow" {
		t.Fatalf("payload = %q, want slow", resp.Payload)
	}
}

// Non-stream: response headers arriving in time disarm the guard even if the body is slow.
func TestFirstByteGuard_ResponseHeadersDisarm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(1500 * time.Millisecond)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	ctx, guard := startFirstByteGuard(context.Background(), time.Second)
	defer guard.release()
	ctx = guard.withResponseHeaderTrace(ctx)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v (guard fired=%v)", err, guard.timedOut())
	}
	if string(body) != "body" || guard.timedOut() {
		t.Fatalf("body=%q timedOut=%v", body, guard.timedOut())
	}
}
