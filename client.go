package appie

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL       = "https://api.ah.nl"
	defaultUserAgent     = "Appie/9.28 (iPhone17,3; iPhone; CPU OS 26_1 like Mac OS X)"
	defaultClientID      = "appie-ios"
	defaultClientVersion = "9.28"
)

// Client is the AH API client. It handles authentication, token management,
// and provides methods to interact with products, orders, shopping lists, and more.
//
// Client is safe for concurrent use. Token state is protected by a mutex.
type Client struct {
	httpClient    *http.Client
	baseURL       string
	userAgent     string
	clientID      string
	clientVersion string

	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	memberID     string
	expiresAt    time.Time
	orderID      string // sent as appie-current-order-id header, mirroring the iOS app; the API may use this (not server-side state) to determine the active order
	orderHash    string

	configPath   string
	loginBaseURL string       // overridable for testing; defaults to "https://login.ah.nl"
	openBrowser  func(string) // overridable for testing; nil uses default
	onLoginURL   func(string) // optional callback, called with the login URL before opening the browser
	logger       *log.Logger
}

// Option configures the client. Use With* functions to create options.
type Option func(*Client)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// WithBaseURL sets a custom base URL.
func WithBaseURL(url string) Option {
	return func(c *Client) {
		c.baseURL = url
	}
}

// WithTokens sets the access and refresh tokens.
func WithTokens(accessToken, refreshToken string) Option {
	return func(c *Client) {
		c.accessToken = accessToken
		c.refreshToken = refreshToken
	}
}

// WithLogger sets a logger for verbose request logging.
func WithLogger(l *log.Logger) Option {
	return func(c *Client) {
		c.logger = l
	}
}

// WithConfigPath sets the path to the config file.
func WithConfigPath(path string) Option {
	return func(c *Client) {
		c.configPath = path
	}
}

// WithOnLoginURL registers a callback that is called with the login URL before
// the browser is opened. Use this to capture or display the URL in your own UI.
func WithOnLoginURL(fn func(string)) Option {
	return func(c *Client) { c.onLoginURL = fn }
}

// New creates a new AH API client.
func New(opts ...Option) *Client {
	c := &Client{
		httpClient:    http.DefaultClient,
		baseURL:       defaultBaseURL,
		userAgent:     defaultUserAgent,
		clientID:      defaultClientID,
		clientVersion: defaultClientVersion,
		logger:        log.New(io.Discard, "", 0),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// DefaultConfigPath returns the standard XDG config path for the appie config
// file ($XDG_CONFIG_HOME/appie/config.json, falling back to ~/.config/appie/config.json).
// If the user's home directory cannot be determined it returns ".appie.json" in cwd.
func DefaultConfigPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ".appie.json"
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "appie", "config.json")
}

// NewWithConfig creates a new client and loads config from the given path.
func NewWithConfig(configPath string, opts ...Option) (*Client, error) {
	c := New(append([]Option{WithConfigPath(configPath)}, opts...)...)

	if err := c.loadConfig(); err != nil {
		if os.IsNotExist(err) {
			return c, nil // Config doesn't exist yet, that's OK
		}
		return nil, err
	}

	return c, nil
}

// loadConfig loads the configuration from the config file.
func (c *Client) loadConfig() error {
	if c.configPath == "" {
		return fmt.Errorf("no config path set")
	}

	data, err := os.ReadFile(c.configPath)
	if err != nil {
		return err
	}

	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	c.mu.Lock()
	c.accessToken = cfg.AccessToken
	c.refreshToken = cfg.RefreshToken
	c.memberID = cfg.MemberID
	c.expiresAt = cfg.ExpiresAt
	c.mu.Unlock()

	return nil
}

// saveConfig saves the current configuration to the config file.
func (c *Client) saveConfig() error {
	if c.configPath == "" {
		return fmt.Errorf("no config path set")
	}

	c.mu.RLock()
	cfg := config{
		AccessToken:  c.accessToken,
		RefreshToken: c.refreshToken,
		MemberID:     c.memberID,
		ExpiresAt:    c.expiresAt,
	}
	c.mu.RUnlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(c.configPath, data, 0600)
}

// IsAuthenticated returns true if the client has an access token.
func (c *Client) IsAuthenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken != ""
}

// setHeaders sets the common headers for API requests.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("x-client-name", c.clientID)
	req.Header.Set("x-client-version", c.clientVersion)
	req.Header.Set("x-application", "AHWEBSHOP")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	c.mu.RLock()
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	if c.orderID != "" {
		req.Header.Set("appie-current-order-id", c.orderID)
		if c.orderHash != "" {
			req.Header.Set("appie-current-order-hash", c.orderHash)
		}
	}
	c.mu.RUnlock()
}

// ensureFreshToken refreshes the access token if it has expired and a refresh token is available.
// Auth endpoints are excluded to avoid infinite loops.
func (c *Client) ensureFreshToken(ctx context.Context, path string) {
	// Don't auto-refresh for auth endpoints
	if strings.HasPrefix(path, "/mobile-auth/") {
		return
	}

	c.mu.RLock()
	expired := !c.expiresAt.IsZero() && time.Now().After(c.expiresAt)
	hasRefresh := c.refreshToken != ""
	c.mu.RUnlock()

	if expired && hasRefresh {
		// Best-effort refresh; if it fails, the original request will proceed
		// with the expired token and the API will return an appropriate error.
		if err := c.refreshAccessToken(ctx); err == nil {
			_ = c.saveConfig()
		}
	}
}

// sendRequest builds, sends, and returns a raw HTTP response. The caller is
// responsible for closing the response body.
func (c *Client) sendRequest(ctx context.Context, method, path string, bodyBytes []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

// decodeResponse reads a response body and decodes it into result (if non-nil).
// Returns an error for HTTP 4xx/5xx statuses.
func decodeResponse(resp *http.Response, result any) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		var apiErr apiError
		if json.Unmarshal(body, &apiErr) == nil && (apiErr.Code != "" || apiErr.Message != "") {
			return &apiErr
		}
		return fmt.Errorf("API error: %d %s", resp.StatusCode, string(body))
	}
	if result != nil && len(body) > 0 {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}
	return nil
}

// DoRequest performs an HTTP request and decodes the response.
func (c *Client) DoRequest(ctx context.Context, method, path string, body, result any) error {
	c.ensureFreshToken(ctx, path)

	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
	}

	start := time.Now()
	resp, err := c.sendRequest(ctx, method, path, bodyBytes)
	if err != nil {
		return err
	}

	// On 401, attempt a token refresh and retry once (skip for auth endpoints to avoid loops).
	if resp.StatusCode == 401 && !strings.HasPrefix(path, "/mobile-auth/") {
		resp, err = c.retryAfterRefresh(ctx, method, path, bodyBytes, resp, &start)
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()

	c.logger.Printf("%s %s %d %s", method, path, resp.StatusCode, time.Since(start).Truncate(time.Millisecond))
	return decodeResponse(resp, result)
}

// retryAfterRefresh closes resp, refreshes the token, and re-sends the request.
// Returns the new response (or the original if refresh is unavailable/fails).
func (c *Client) retryAfterRefresh(ctx context.Context, method, path string, bodyBytes []byte, resp *http.Response, start *time.Time) (*http.Response, error) {
	c.mu.RLock()
	hasRefresh := c.refreshToken != ""
	c.mu.RUnlock()
	if !hasRefresh {
		return resp, nil
	}
	resp.Body.Close()
	if err := c.refreshAccessToken(ctx); err != nil {
		// Refresh failed; caller will still decode the (closed) response — open a
		// fresh one so the deferred Body.Close() and decodeResponse have a valid body.
		return c.sendRequest(ctx, method, path, bodyBytes)
	}
	_ = c.saveConfig()
	*start = time.Now()
	return c.sendRequest(ctx, method, path, bodyBytes)
}

// DoGraphQL performs a GraphQL request.
func (c *Client) DoGraphQL(ctx context.Context, query string, variables map[string]any, result any) error {
	req := graphQLRequest{
		Query:     query,
		Variables: variables,
	}

	var resp graphQLResponse[json.RawMessage]
	if err := c.DoRequest(ctx, http.MethodPost, "/graphql", req, &resp); err != nil {
		return err
	}

	if len(resp.Errors) > 0 {
		return fmt.Errorf("graphql error: %s", resp.Errors[0].Message)
	}

	if result != nil {
		if err := json.Unmarshal(resp.Data, result); err != nil {
			return fmt.Errorf("failed to decode graphql response: %w", err)
		}
	}

	return nil
}
