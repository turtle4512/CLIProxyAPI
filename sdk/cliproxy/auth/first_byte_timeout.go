package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

const (
	// defaultFirstByteTimeout applies when first-byte-timeout-seconds is omitted.
	defaultFirstByteTimeout = 25 * time.Second
	// firstByteTimeoutCooldown is the short model cooldown applied after a first-byte
	// timeout so follow-up requests skip the stalled upstream briefly instead of paying
	// the full timeout again, without parking it for the long transient cooldown.
	firstByteTimeoutCooldown = 30 * time.Second

	// ErrorCodeFirstByteTimeout marks attempts abandoned because the upstream produced
	// no first byte within the configured window.
	ErrorCodeFirstByteTimeout = "first_byte_timeout"
)

// firstByteTimeoutFromConfig resolves the configured first-byte timeout.
// Omitted -> default (25s); 0 or negative -> disabled.
func firstByteTimeoutFromConfig(cfg *internalconfig.Config) time.Duration {
	if cfg == nil || cfg.FirstByteTimeoutSeconds == nil {
		return defaultFirstByteTimeout
	}
	if *cfg.FirstByteTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(*cfg.FirstByteTimeoutSeconds) * time.Second
}

// isOpenAICompatAuth reports whether the auth was synthesized from an openai-compatibility block.
func isOpenAICompatAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	return auth.Attributes != nil && strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

// firstByteTimeoutPlan describes whether the first-byte guard should be armed for an attempt.
type firstByteTimeoutPlan struct {
	timeout time.Duration
	// hasAlternativeAuth reports whether another credential/provider can still be tried
	// after this one. Pooled upstream models inside the same auth are handled separately.
	hasAlternativeAuth bool
}

type firstByteTimeoutPlanKey struct{}

func withFirstByteTimeoutPlan(ctx context.Context, plan firstByteTimeoutPlan) context.Context {
	if plan.timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, firstByteTimeoutPlanKey{}, plan)
}

func firstByteTimeoutPlanFromContext(ctx context.Context) firstByteTimeoutPlan {
	if ctx == nil {
		return firstByteTimeoutPlan{}
	}
	plan, _ := ctx.Value(firstByteTimeoutPlanKey{}).(firstByteTimeoutPlan)
	return plan
}

// armFor reports whether the guard should be armed for the model at idx of n pooled models.
func (p firstByteTimeoutPlan) armFor(idx, n int) bool {
	if p.timeout <= 0 {
		return false
	}
	return p.hasAlternativeAuth || idx < n-1
}

// firstByteTimeoutPlanFor builds the plan for one openai-compatibility attempt. The guard is
// only meaningful when a fallback exists, otherwise a slow-but-only upstream would just fail.
func (m *Manager) firstByteTimeoutPlanFor(auth *Auth, homeMode bool, providers []string, routeModel string, tried map[string]struct{}, attempted, maxRetryCredentials int) firstByteTimeoutPlan {
	if m == nil || homeMode || !isOpenAICompatAuth(auth) {
		return firstByteTimeoutPlan{}
	}
	timeout := firstByteTimeoutFromConfig(m.runtimeConfigSnapshot())
	if timeout <= 0 {
		return firstByteTimeoutPlan{}
	}
	hasAlt := false
	if maxRetryCredentials <= 0 || attempted < maxRetryCredentials {
		hasAlt = m.hasUntriedCandidate(providers, routeModel, tried)
	}
	return firstByteTimeoutPlan{timeout: timeout, hasAlternativeAuth: hasAlt}
}

// hasUntriedCandidate reports whether at least one other enabled, non-cooling auth for the
// requested providers supports routeModel and has not been tried in this request.
// Mirrors the candidate filter of pickNextMixedLegacy (read lock only, no cloning).
func (m *Manager) hasUntriedCandidate(providers []string, routeModel string, tried map[string]struct{}) bool {
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if p := strings.TrimSpace(strings.ToLower(provider)); p != "" {
			providerSet[p] = struct{}{}
		}
	}
	if len(providerSet) == 0 {
		return false
	}
	now := time.Now()
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		providerKey := executorKeyFromAuth(candidate)
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if strings.TrimSpace(routeModel) != "" && !m.authSupportsRouteModel(registryRef, candidate, routeModel) {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(candidate, m.selectionModelForAuth(candidate, routeModel), now); blocked {
			continue
		}
		return true
	}
	return false
}

// firstByteGuard cancels an upstream attempt when no first byte arrives in time.
type firstByteGuard struct {
	timeout time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	fired   atomic.Bool
}

// startFirstByteGuard derives an attempt context that is cancelled after timeout unless
// markFirstByte is called first. A nil guard is returned (and ctx unchanged) when disabled.
func startFirstByteGuard(ctx context.Context, timeout time.Duration) (context.Context, *firstByteGuard) {
	if timeout <= 0 {
		return ctx, nil
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	g := &firstByteGuard{timeout: timeout, cancel: cancel}
	g.timer = time.AfterFunc(timeout, func() {
		g.fired.Store(true)
		cancel()
	})
	return attemptCtx, g
}

// withResponseHeaderTrace stops the guard as soon as the first response byte (status line /
// headers) is read from the upstream connection. Used for non-stream requests.
func (g *firstByteGuard) withResponseHeaderTrace(ctx context.Context) context.Context {
	if g == nil {
		return ctx
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotFirstResponseByte: func() { g.markFirstByte() },
	})
}

// markFirstByte disarms the timer. It returns false when the timeout already fired.
func (g *firstByteGuard) markFirstByte() bool {
	if g == nil {
		return true
	}
	if g.timer.Stop() {
		return true
	}
	return !g.fired.Load()
}

// timedOut reports whether the guard fired.
func (g *firstByteGuard) timedOut() bool {
	return g != nil && g.fired.Load()
}

// release stops the timer and cancels the attempt context.
func (g *firstByteGuard) release() {
	if g == nil {
		return
	}
	g.timer.Stop()
	g.cancel()
}

// err builds the retryable error reported when the guard fired.
func (g *firstByteGuard) err() error {
	timeout := time.Duration(0)
	if g != nil {
		timeout = g.timeout
	}
	return &Error{
		Code:       ErrorCodeFirstByteTimeout,
		Message:    fmt.Sprintf("upstream produced no first byte within %s", timeout),
		Retryable:  true,
		HTTPStatus: http.StatusGatewayTimeout,
	}
}

func isFirstByteTimeoutResultError(err *Error) bool {
	return err != nil && err.Code == ErrorCodeFirstByteTimeout
}
