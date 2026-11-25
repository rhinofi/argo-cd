package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	utilio "github.com/argoproj/argo-cd/v3/util/io"
)

const (
	envGitHubTokenFactoryURL       = "ARGOCD_GITHUB_TOKEN_FACTORY_URL"
	envGitHubTokenFactoryInvokerSA = "ARGOCD_GITHUB_TOKEN_FACTORY_INVOKER_SA"
	envGitHubTokenFactoryRepoURL   = "ARGOCD_GITHUB_TOKEN_FACTORY_REPO_URL"
	envGoogleOAuthAccessToken      = "GOOGLE_OAUTH_ACCESS_TOKEN"

	defaultIAMCredentialsBaseURL = "https://iamcredentials.googleapis.com/v1"
	tokenFactoryRefreshMargin    = 5 * time.Minute
	tokenFactoryFailureBackoff   = 15 * time.Second
	tokenFactoryRequestTimeout   = 30 * time.Second
	tokenFactoryMaxBodyBytes     = 1 << 20
	cloudPlatformScope           = "https://www.googleapis.com/auth/cloud-platform"
)

type tokenFactoryCacheEntry struct {
	token     string
	expiresAt time.Time
}

type tokenFactoryInFlight struct {
	done  chan struct{}
	entry tokenFactoryCacheEntry
	err   error
}

type tokenFactoryTokenCache struct {
	mu         sync.Mutex
	entries    map[string]tokenFactoryCacheEntry
	inFlight   map[string]*tokenFactoryInFlight
	retryAfter map[string]time.Time
}

func newTokenFactoryTokenCache() *tokenFactoryTokenCache {
	return &tokenFactoryTokenCache{
		entries:    make(map[string]tokenFactoryCacheEntry),
		inFlight:   make(map[string]*tokenFactoryInFlight),
		retryAfter: make(map[string]time.Time),
	}
}

var (
	defaultTokenFactoryCache = newTokenFactoryTokenCache()

	defaultGCPTokenSourceMu sync.Mutex
	defaultGCPTokenSource   oauth2.TokenSource
)

func getDefaultGCPAccessToken(ctx context.Context) (string, error) {
	// Local runs outside GKE may supply a pre-minted OAuth access token via
	// GOOGLE_OAUTH_ACCESS_TOKEN; unset in-cluster where Workload Identity ADC is used.
	if fixed := strings.TrimSpace(os.Getenv(envGoogleOAuthAccessToken)); fixed != "" {
		return fixed, nil
	}

	defaultGCPTokenSourceMu.Lock()
	ts := defaultGCPTokenSource
	if ts == nil {
		var err error
		ts, err = google.DefaultTokenSource(ctx, cloudPlatformScope)
		if err != nil {
			defaultGCPTokenSourceMu.Unlock()
			return "", fmt.Errorf("failed to initialize GCP default token source: %w", err)
		}
		defaultGCPTokenSource = ts
	}
	defaultGCPTokenSourceMu.Unlock()

	tok, err := ts.Token()
	if err != nil {
		return "", fmt.Errorf("failed to obtain GCP access token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("GCP default token source returned empty access token")
	}
	return tok.AccessToken, nil
}

type gcpAccessTokenFunc func(ctx context.Context) (string, error)

var _ Creds = TokenFactoryCreds{}

// TokenFactoryCreds authenticates to GitHub using short-lived installation
// tokens minted on demand from Rhino's token-factory-github Cloud Run service
// via GKE Workload Identity.
type TokenFactoryCreds struct {
	factoryURL            string
	invokerSA             string
	store                 CredsStore
	httpClient            *http.Client
	iamCredentialsBaseURL string
	gcpAccessToken        gcpAccessTokenFunc
	cache                 *tokenFactoryTokenCache
	refreshMargin         time.Duration
	failureBackoff        time.Duration
	now                   func() time.Time
}

func newTokenFactoryCreds(
	factoryURL string,
	invokerSA string,
	store CredsStore,
	httpClient *http.Client,
	iamCredentialsBaseURL string,
	gcpAccessToken gcpAccessTokenFunc,
	cache *tokenFactoryTokenCache,
	refreshMargin time.Duration,
	failureBackoff time.Duration,
	now func() time.Time,
) TokenFactoryCreds {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: tokenFactoryRequestTimeout}
	}
	if iamCredentialsBaseURL == "" {
		iamCredentialsBaseURL = defaultIAMCredentialsBaseURL
	}
	if gcpAccessToken == nil {
		gcpAccessToken = getDefaultGCPAccessToken
	}
	if cache == nil {
		cache = defaultTokenFactoryCache
	}
	if refreshMargin <= 0 {
		refreshMargin = tokenFactoryRefreshMargin
	}
	if failureBackoff <= 0 {
		failureBackoff = tokenFactoryFailureBackoff
	}
	if now == nil {
		now = time.Now
	}
	return TokenFactoryCreds{
		factoryURL:            strings.TrimRight(factoryURL, "/"),
		invokerSA:             invokerSA,
		store:                 store,
		httpClient:            httpClient,
		iamCredentialsBaseURL: strings.TrimRight(iamCredentialsBaseURL, "/"),
		gcpAccessToken:        gcpAccessToken,
		cache:                 cache,
		refreshMargin:         refreshMargin,
		failureBackoff:        failureBackoff,
		now:                   now,
	}
}

// NewTokenFactoryCredsForRepo returns TokenFactoryCreds when the token factory
// environment variables are configured and repoURL matches the configured
// repository URL.
func NewTokenFactoryCredsForRepo(repoURL string, store CredsStore) (TokenFactoryCreds, bool) {
	factoryURL := strings.TrimSpace(os.Getenv(envGitHubTokenFactoryURL))
	invokerSA := strings.TrimSpace(os.Getenv(envGitHubTokenFactoryInvokerSA))
	configuredRepoURL := strings.TrimSpace(os.Getenv(envGitHubTokenFactoryRepoURL))
	if factoryURL == "" || invokerSA == "" || configuredRepoURL == "" {
		return TokenFactoryCreds{}, false
	}
	parsed, err := url.Parse(factoryURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		log.Warnf("Ignoring invalid %s: must be an https URL", envGitHubTokenFactoryURL)
		return TokenFactoryCreds{}, false
	}
	if !SameURL(repoURL, configuredRepoURL) {
		return TokenFactoryCreds{}, false
	}
	return newTokenFactoryCreds(
		factoryURL,
		invokerSA,
		store,
		nil,
		defaultIAMCredentialsBaseURL,
		getDefaultGCPAccessToken,
		defaultTokenFactoryCache,
		tokenFactoryRefreshMargin,
		tokenFactoryFailureBackoff,
		time.Now,
	), true
}

func (c TokenFactoryCreds) GetUserInfo(_ context.Context) (string, string, error) {
	return githubAccessTokenUsername, "", nil
}

func (c TokenFactoryCreds) Environ() (io.Closer, []string, error) {
	token, err := c.getAccessToken()
	if err != nil {
		return NopCloser{}, nil, err
	}
	nonce := c.store.Add(githubAccessTokenUsername, token)
	env := c.store.Environ(nonce)
	return utilio.NewCloser(func() error {
		c.store.Remove(nonce)
		return nil
	}), env, nil
}

func (c TokenFactoryCreds) cacheKey() string {
	return c.factoryURL + "\x00" + c.invokerSA
}

func (c TokenFactoryCreds) getAccessToken() (string, error) {
	key := c.cacheKey()

	c.cache.mu.Lock()
	now := c.now()
	cached, hasCached := c.cache.entries[key]
	if hasCached && now.Add(c.refreshMargin).Before(cached.expiresAt) {
		c.cache.mu.Unlock()
		return cached.token, nil
	}
	if hasCached && now.Before(cached.expiresAt) {
		if retryAt, hasBackoff := c.cache.retryAfter[key]; hasBackoff && now.Before(retryAt) {
			c.cache.mu.Unlock()
			return cached.token, nil
		}
	}

	if call, ok := c.cache.inFlight[key]; ok {
		c.cache.mu.Unlock()
		<-call.done
		return c.resolveInFlightResult(key, call)
	}

	call := &tokenFactoryInFlight{done: make(chan struct{})}
	c.cache.inFlight[key] = call
	c.cache.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), tokenFactoryRequestTimeout)
	defer cancel()

	var (
		minted tokenFactoryCacheEntry
		err    error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("token factory refresh panicked: %v", r)
			}
			c.cache.mu.Lock()
			delete(c.cache.inFlight, key)
			afterNow := c.now()
			if err == nil {
				c.cache.entries[key] = minted
				delete(c.cache.retryAfter, key)
				call.entry = minted
			} else {
				call.err = err
				if cur, ok := c.cache.entries[key]; ok && afterNow.Before(cur.expiresAt) {
					nextRetry := afterNow.Add(c.failureBackoff)
					c.cache.retryAfter[key] = nextRetry
					log.Warnf(
						"GitHub token factory refresh failed, reusing cached token until %s (backing off until %s): %v",
						cur.expiresAt.Format(time.RFC3339),
						nextRetry.Format(time.RFC3339),
						err,
					)
				}
			}
			close(call.done)
			c.cache.mu.Unlock()
		}()

		minted, err = c.mint(ctx)
	}()

	return c.resolveInFlightResult(key, call)
}

func (c TokenFactoryCreds) resolveInFlightResult(key string, call *tokenFactoryInFlight) (string, error) {
	if call.err == nil {
		if c.now().Before(call.entry.expiresAt) {
			return call.entry.token, nil
		}
		return "", fmt.Errorf("token factory returned already-expired token (%s)", call.entry.expiresAt.Format(time.RFC3339))
	}

	c.cache.mu.Lock()
	cached, hasCached := c.cache.entries[key]
	c.cache.mu.Unlock()

	if hasCached && c.now().Before(cached.expiresAt) {
		return cached.token, nil
	}
	return "", call.err
}

func (c TokenFactoryCreds) mint(ctx context.Context) (tokenFactoryCacheEntry, error) {
	accessToken, err := c.gcpAccessToken(ctx)
	if err != nil {
		return tokenFactoryCacheEntry{}, err
	}

	idToken, err := c.mintIDToken(ctx, accessToken)
	if err != nil {
		return tokenFactoryCacheEntry{}, err
	}

	return c.mintGitHubToken(ctx, idToken)
}

func (c TokenFactoryCreds) mintIDToken(ctx context.Context, accessToken string) (string, error) {
	endpoint := fmt.Sprintf(
		"%s/projects/-/serviceAccounts/%s:generateIdToken",
		c.iamCredentialsBaseURL,
		url.PathEscape(c.invokerSA),
	)
	payload, err := json.Marshal(struct {
		Audience     string `json:"audience"`
		IncludeEmail bool   `json:"includeEmail"`
	}{
		Audience:     c.factoryURL,
		IncludeEmail: true,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal generateIdToken request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("failed to build generateIdToken request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("generateIdToken request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenFactoryMaxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to read generateIdToken response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("generateIdToken returned status %d", resp.StatusCode)
	}

	var decoded struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("failed to decode generateIdToken response: %w", err)
	}
	if decoded.Token == "" {
		return "", errors.New("generateIdToken returned empty token")
	}
	return decoded.Token, nil
}

func (c TokenFactoryCreds) mintGitHubToken(ctx context.Context, idToken string) (tokenFactoryCacheEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.factoryURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return tokenFactoryCacheEntry{}, fmt.Errorf("failed to build token factory request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+idToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return tokenFactoryCacheEntry{}, fmt.Errorf("token factory request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenFactoryMaxBodyBytes))
	if err != nil {
		return tokenFactoryCacheEntry{}, fmt.Errorf("failed to read token factory response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenFactoryCacheEntry{}, fmt.Errorf("token factory returned status %d", resp.StatusCode)
	}

	var decoded struct {
		Token               string `json:"token"`
		ExpiresAt           string `json:"expiresAt"`
		RepositorySelection string `json:"repositorySelection"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return tokenFactoryCacheEntry{}, fmt.Errorf("failed to decode token factory response: %w", err)
	}
	if decoded.Token == "" {
		return tokenFactoryCacheEntry{}, errors.New("token factory returned empty token")
	}
	expiresAt, err := time.Parse(time.RFC3339, decoded.ExpiresAt)
	if err != nil {
		return tokenFactoryCacheEntry{}, fmt.Errorf("token factory returned invalid expiresAt: %w", err)
	}
	if !expiresAt.After(c.now()) {
		return tokenFactoryCacheEntry{}, fmt.Errorf("token factory returned already-expired token (%s)", expiresAt.Format(time.RFC3339))
	}

	log.Infof(
		"Minted GitHub installation token from token factory (repositorySelection=%s, expiresAt=%s)",
		decoded.RepositorySelection,
		expiresAt.Format(time.RFC3339),
	)
	return tokenFactoryCacheEntry{
		token:     decoded.Token,
		expiresAt: expiresAt,
	}, nil
}
