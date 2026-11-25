package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingCredsStore struct {
	mu      sync.Mutex
	added   map[string][2]string
	removed []string
	nextID  int
}

func newRecordingCredsStore() *recordingCredsStore {
	return &recordingCredsStore{added: make(map[string][2]string)}
}

func (s *recordingCredsStore) Add(username, password string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("nonce-%d", s.nextID)
	s.added[id] = [2]string{username, password}
	return id
}

func (s *recordingCredsStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, id)
}

func (s *recordingCredsStore) Environ(id string) []string {
	return []string{"ARGOCD_ASK_PASS_NONCE=" + id}
}

func TestTokenFactoryCredsForRepoMatching(t *testing.T) {
	t.Run("disabled when env vars unset", func(t *testing.T) {
		t.Setenv(envGitHubTokenFactoryURL, "")
		t.Setenv(envGitHubTokenFactoryInvokerSA, "")
		t.Setenv(envGitHubTokenFactoryRepoURL, "")

		_, ok := NewTokenFactoryCredsForRepo("https://github.com/rhinofi/dvf-k8s-manifests-dev.git", NoopCredsStore{})
		assert.False(t, ok)
	})

	t.Run("matches repo URL with or without .git suffix", func(t *testing.T) {
		t.Setenv(envGitHubTokenFactoryURL, "https://github-argo-git-dev.run.app")
		t.Setenv(envGitHubTokenFactoryInvokerSA, "pam-protected-sa@rhino-gh-argo-git-dev-invoker.iam.gserviceaccount.com")
		t.Setenv(envGitHubTokenFactoryRepoURL, "https://github.com/rhinofi/dvf-k8s-manifests-dev.git")

		_, ok := NewTokenFactoryCredsForRepo("https://github.com/rhinofi/dvf-k8s-manifests-dev", NoopCredsStore{})
		assert.True(t, ok)

		_, ok = NewTokenFactoryCredsForRepo("https://github.com/rhinofi/dvf-k8s-manifests-prd.git", NoopCredsStore{})
		assert.False(t, ok)
	})

	t.Run("rejects non-https factory URL", func(t *testing.T) {
		t.Setenv(envGitHubTokenFactoryURL, "http://github-argo-git-dev.run.app")
		t.Setenv(envGitHubTokenFactoryInvokerSA, "pam-protected-sa@rhino-gh-argo-git-dev-invoker.iam.gserviceaccount.com")
		t.Setenv(envGitHubTokenFactoryRepoURL, "https://github.com/rhinofi/dvf-k8s-manifests-dev.git")

		_, ok := NewTokenFactoryCredsForRepo("https://github.com/rhinofi/dvf-k8s-manifests-dev.git", NoopCredsStore{})
		assert.False(t, ok)
	})
}

func TestTokenFactoryCredsMintCacheRefreshAndFallback(t *testing.T) {
	currentTime := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var timeMu sync.Mutex
	now := func() time.Time {
		timeMu.Lock()
		defer timeMu.Unlock()
		return currentTime
	}
	advance := func(d time.Duration) {
		timeMu.Lock()
		defer timeMu.Unlock()
		currentTime = currentTime.Add(d)
	}

	var idTokenCalls atomic.Int32
	var factoryCalls atomic.Int32
	var failFactory atomic.Bool

	invokerSA := "pam-protected-sa@rhino-gh-argo-git-dev-invoker.iam.gserviceaccount.com"
	var factoryServer *httptest.Server

	iamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idTokenCalls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/projects/-/serviceAccounts/"+invokerSA+":generateIdToken", r.URL.Path)
		assert.Equal(t, "Bearer gcp-access-token", r.Header.Get("Authorization"))

		var reqBody struct {
			Audience     string `json:"audience"`
			IncludeEmail bool   `json:"includeEmail"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Equal(t, factoryServer.URL, reqBody.Audience)
		assert.True(t, reqBody.IncludeEmail)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "oidc-id-token"})
	}))
	defer iamServer.Close()

	factoryServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := factoryCalls.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "Bearer oidc-id-token", r.Header.Get("Authorization"))

		if failFactory.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":               fmt.Sprintf("ghs_token_%d", n),
			"expiresAt":           now().Add(time.Hour).Format(time.RFC3339),
			"repositorySelection": "selected",
		})
	}))
	defer factoryServer.Close()

	store := newRecordingCredsStore()
	creds := newTokenFactoryCreds(
		factoryServer.URL,
		invokerSA,
		store,
		factoryServer.Client(),
		iamServer.URL,
		func(_ context.Context) (string, error) { return "gcp-access-token", nil },
		newTokenFactoryTokenCache(),
		now,
	)

	// Initial mint through Environ()
	closer, env, err := creds.Environ()
	require.NoError(t, err)
	assert.Equal(t, []string{"ARGOCD_ASK_PASS_NONCE=nonce-1"}, env)
	assert.Equal(t, [2]string{"x-access-token", "ghs_token_1"}, store.added["nonce-1"])
	require.NoError(t, closer.Close())
	assert.Equal(t, []string{"nonce-1"}, store.removed)
	assert.Equal(t, int32(1), idTokenCalls.Load())
	assert.Equal(t, int32(1), factoryCalls.Load())

	// Cached hit through go-git newAuth() at t+50m (outside 5m refresh margin)
	advance(50 * time.Minute)
	auth, err := newAuth("https://github.com/rhinofi/dvf-k8s-manifests-dev.git", creds)
	require.NoError(t, err)
	basicAuth, ok := auth.(*githttp.BasicAuth)
	require.True(t, ok)
	assert.Equal(t, "x-access-token", basicAuth.Username)
	assert.Equal(t, "ghs_token_1", basicAuth.Password)
	assert.Equal(t, int32(1), factoryCalls.Load())

	// Inside 5m refresh margin (t+56m) with transient failure: falls back to unexpired cached token
	advance(6 * time.Minute)
	failFactory.Store(true)
	tok, err := creds.getAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ghs_token_1", tok)
	assert.Equal(t, int32(2), factoryCalls.Load())

	// Within failure backoff (t+56m05s): immediately returns cached token without calling factory
	advance(5 * time.Second)
	tok, err = creds.getAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ghs_token_1", tok)
	assert.Equal(t, int32(2), factoryCalls.Load())

	// Past failure backoff (t+56m20s) with factory recovered: mints fresh token
	advance(15 * time.Second)
	failFactory.Store(false)
	tok, err = creds.getAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ghs_token_3", tok)
	assert.Equal(t, int32(3), factoryCalls.Load())

	// Past expiry with factory failing: returns error instead of expired token
	advance(61 * time.Minute)
	failFactory.Store(true)
	_, err = creds.getAccessToken()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 503")
}

func TestTokenFactoryCredsConcurrentDeduplication(t *testing.T) {
	var factoryCalls atomic.Int32
	invokerSA := "pam-protected-sa@rhino-gh-argo-git-dev-invoker.iam.gserviceaccount.com"

	iamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "oidc-id-token"})
	}))
	defer iamServer.Close()

	factoryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := factoryCalls.Add(1)
		time.Sleep(25 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":               fmt.Sprintf("ghs_concurrent_%d", n),
			"expiresAt":           time.Now().Add(time.Hour).Format(time.RFC3339),
			"repositorySelection": "selected",
		})
	}))
	defer factoryServer.Close()

	creds := newTokenFactoryCreds(
		factoryServer.URL,
		invokerSA,
		NoopCredsStore{},
		factoryServer.Client(),
		iamServer.URL,
		func(_ context.Context) (string, error) { return "gcp-access-token", nil },
		newTokenFactoryTokenCache(),
		time.Now,
	)

	const goroutines = 12
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tokens[idx], errs[idx] = creds.getAccessToken()
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(1), factoryCalls.Load())
	for i := range goroutines {
		require.NoError(t, errs[i])
		assert.Equal(t, "ghs_concurrent_1", tokens[i])
	}
}

func TestTokenFactoryCredsConcurrentFailureSharesResultAndBacksOff(t *testing.T) {
	currentTime := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var timeMu sync.Mutex
	now := func() time.Time {
		timeMu.Lock()
		defer timeMu.Unlock()
		return currentTime
	}
	advance := func(d time.Duration) {
		timeMu.Lock()
		defer timeMu.Unlock()
		currentTime = currentTime.Add(d)
	}

	var factoryCalls atomic.Int32
	var failFactory atomic.Bool
	invokerSA := "pam-protected-sa@rhino-gh-argo-git-dev-invoker.iam.gserviceaccount.com"

	iamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "oidc-id-token"})
	}))
	defer iamServer.Close()

	factoryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := factoryCalls.Add(1)
		if failFactory.Load() {
			time.Sleep(30 * time.Millisecond)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":               fmt.Sprintf("ghs_shared_%d", n),
			"expiresAt":           now().Add(time.Hour).Format(time.RFC3339),
			"repositorySelection": "selected",
		})
	}))
	defer factoryServer.Close()

	creds := newTokenFactoryCreds(
		factoryServer.URL,
		invokerSA,
		NoopCredsStore{},
		factoryServer.Client(),
		iamServer.URL,
		func(_ context.Context) (string, error) { return "gcp-access-token", nil },
		newTokenFactoryTokenCache(),
		now,
	)

	// Populate cache
	tok, err := creds.getAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ghs_shared_1", tok)
	assert.Equal(t, int32(1), factoryCalls.Load())

	// Enter refresh window while factory is failing: 12 concurrent callers should
	// share a single failed refresh attempt and all fall back to the cached token.
	advance(56 * time.Minute)
	failFactory.Store(true)

	const goroutines = 12
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tokens[idx], errs[idx] = creds.getAccessToken()
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(2), factoryCalls.Load())
	for i := range goroutines {
		require.NoError(t, errs[i])
		assert.Equal(t, "ghs_shared_1", tokens[i])
	}

	// Follow-up caller during backoff does not hit the factory again
	advance(10 * time.Second)
	tok, err = creds.getAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ghs_shared_1", tok)
	assert.Equal(t, int32(2), factoryCalls.Load())

	// After backoff expires, a new attempt is made
	advance(10 * time.Second)
	tok, err = creds.getAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ghs_shared_1", tok)
	assert.Equal(t, int32(3), factoryCalls.Load())
}

func TestTokenFactoryCredsFallbackRechecksExpiryAfterFailedRefresh(t *testing.T) {
	currentTime := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var timeMu sync.Mutex
	now := func() time.Time {
		timeMu.Lock()
		defer timeMu.Unlock()
		return currentTime
	}
	advance := func(d time.Duration) {
		timeMu.Lock()
		defer timeMu.Unlock()
		currentTime = currentTime.Add(d)
	}

	var failFactory atomic.Bool
	invokerSA := "pam-protected-sa@rhino-gh-argo-git-dev-invoker.iam.gserviceaccount.com"

	iamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "oidc-id-token"})
	}))
	defer iamServer.Close()

	factoryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failFactory.Load() {
			// Simulate a slow refresh attempt during which the cached token expires
			advance(20 * time.Second)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":               "ghs_expiring_1",
			"expiresAt":           now().Add(time.Hour).Format(time.RFC3339),
			"repositorySelection": "selected",
		})
	}))
	defer factoryServer.Close()

	creds := newTokenFactoryCreds(
		factoryServer.URL,
		invokerSA,
		NoopCredsStore{},
		factoryServer.Client(),
		iamServer.URL,
		func(_ context.Context) (string, error) { return "gcp-access-token", nil },
		newTokenFactoryTokenCache(),
		now,
	)

	tok, err := creds.getAccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ghs_expiring_1", tok)

	// 10s left before expiry; the failed refresh advances time by 20s (past expiry).
	// Fallback must recheck expiry after the attempt and return the error instead of an expired token.
	advance(59*time.Minute + 50*time.Second)
	failFactory.Store(true)
	_, err = creds.getAccessToken()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 503")
}

func TestTokenFactoryDefaultGCPAccessTokenFromEnv(t *testing.T) {
	t.Setenv(envGoogleOAuthAccessToken, "ya29.test-access-token")

	tok, err := getDefaultGCPAccessToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ya29.test-access-token", tok)
}
