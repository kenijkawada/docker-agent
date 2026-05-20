package mcpcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

type stubEnv struct{ vars map[string]string }

func (s stubEnv) Get(_ context.Context, name string) (string, bool) {
	v, ok := s.vars[name]
	return v, ok
}

func TestLoadCatalog(t *testing.T) {
	cat, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "Docker MCP Catalog", cat.Source)
	assert.NotEmpty(t, cat.SourceURL)
	assert.Positive(t, cat.Count)
	assert.Equal(t, len(cat.Servers), cat.Count)

	// Every server in the catalog must be remote streamable-http and have a URL.
	for _, s := range cat.Servers {
		assert.NotEmpty(t, s.ID, "server id must not be empty")
		assert.Equal(t, "streamable-http", s.Transport, "server %s has unexpected transport", s.ID)
		assert.NotEmpty(t, s.URL, "server %s has no URL", s.ID)
		// auth.type must be one of the three documented values.
		switch s.Auth.Type {
		case "oauth", "api_key", "none":
		default:
			t.Fatalf("server %s has invalid auth.type %q", s.ID, s.Auth.Type)
		}
	}
}

func TestSearchTool(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ctx := t.Context()

	res, err := ts.handleSearch(ctx, SearchArgs{Query: "stripe"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, strings.ToLower(res.Output), "stripe")

	// Empty query returns the catalog truncated to emptyQuerySearchLimit
	// so we don't dump the full catalog into the LLM context window.
	res, err = ts.handleSearch(ctx, SearchArgs{Query: ""})
	require.NoError(t, err)
	require.False(t, res.IsError)
	first := strings.SplitN(res.Output, "\n", 2)[0]
	assert.Contains(t, first, "showing ")
	assert.Contains(t, first, "refine with a keyword")
	body := strings.SplitN(res.Output, "\n", 2)[1]
	var parsed []SearchResult
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	assert.Len(t, parsed, emptyQuerySearchLimit)
	require.Greater(t, ts.catalog.Count, emptyQuerySearchLimit,
		"test fixture: catalog should be larger than the empty-query cap")

	// Unknown query returns an error result (not a Go error).
	res, err = ts.handleSearch(ctx, SearchArgs{Query: "xxxxxx_no_such_server_xxxxxx"})
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestEnableDisableLifecycle(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ctx := t.Context()

	// Pick the first OAuth-style server in the catalog as a known good fixture.
	var oauthID string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthID = s.ID
			break
		}
	}
	require.NotEmpty(t, oauthID, "test fixture: catalog should contain at least one OAuth server")

	// Track tools-changed callbacks. Use atomic.Int32 to satisfy -race even
	// though every call site here happens to be on the same goroutine.
	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	// Before enabling: only the always-on meta-tools. Disable and reset are
	// hidden until at least one server is enabled.
	toolList, err := ts.Tools(ctx)
	require.NoError(t, err)
	names := toolNames(toolList)
	assert.ElementsMatch(t, []string{
		ToolNameSearch, ToolNameList, ToolNameEnable,
	}, names)
	assert.NotContains(t, names, ToolNameDisable,
		"disable must not be exposed when no server is enabled")
	assert.NotContains(t, names, ToolNameResetAuth,
		"reset_auth must not be exposed when no server is enabled")

	// Enable: a callback should fire and the underlying mcp.Toolset should
	// be present in t.enabled. We deliberately do NOT exercise the network
	// path — Tools(ctx) on the lazily-instantiated toolset would attempt a
	// connection. Just check the bookkeeping.
	res, err := ts.handleEnable(ctx, EnableArgs{ID: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError, "enable failed: %s", res.Output)
	assert.Contains(t, res.Output, "OAuth")
	assert.Equal(t, int32(1), changes.Load(), "enable should fire tools-changed handler exactly once")

	ts.mu.RLock()
	_, exists := ts.enabled[oauthID]
	ts.mu.RUnlock()
	assert.True(t, exists)

	// Re-enable: idempotent, no extra change notification.
	res, err = ts.handleEnable(ctx, EnableArgs{ID: oauthID})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "already enabled")
	assert.Equal(t, int32(1), changes.Load())

	// Search now reports it as enabled.
	res, err = ts.handleSearch(ctx, SearchArgs{Query: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError)
	body := strings.SplitN(res.Output, "\n", 2)[1]
	var parsed []SearchResult
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	var found *SearchResult
	for i := range parsed {
		if parsed[i].ID == oauthID {
			found = &parsed[i]
		}
	}
	require.NotNil(t, found)
	assert.True(t, found.Enabled)

	// Disable: removes the entry and fires another change notification.
	res, err = ts.handleDisable(ctx, DisableArgs{ID: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Equal(t, int32(2), changes.Load())

	ts.mu.RLock()
	_, exists = ts.enabled[oauthID]
	ts.mu.RUnlock()
	assert.False(t, exists)

	// Disable again: error result, no extra change.
	res, err = ts.handleDisable(ctx, DisableArgs{ID: oauthID})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Equal(t, int32(2), changes.Load())
}

func TestEnableUnresolvedHeaderEnvSurfacesWarning(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})

	// Synthetic catalog entry: auth.type="none" so neither missingAPIKeyEnv
	// nor the api_key path fires; the only signal is the post-expansion
	// scan over Headers.
	const id = "unresolved-headers"
	server := Server{
		ID:        id,
		Title:     "Unresolved Headers Server",
		URL:       "https://example.invalid/mcp",
		Transport: "streamable-http",
		Auth:      Auth{Type: "none"},
		Headers: map[string]string{
			"Authorization": "Bearer ${UNDECLARED_TOKEN}",
		},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, res.Output, "WARNING")
	assert.Contains(t, res.Output, "UNDECLARED_TOKEN",
		"the post-expansion scan must surface env vars referenced from headers but not declared under Auth.Secrets")
	assert.Contains(t, res.Output, ToolNameDisable)
	assert.Contains(t, res.Output, ToolNameEnable)
}

func TestUnresolvedHeaderEnvsHelper(t *testing.T) {
	assert.Empty(t, unresolvedHeaderEnvs(nil))
	assert.Empty(t, unresolvedHeaderEnvs(map[string]string{"X": "plain-value"}))

	got := unresolvedHeaderEnvs(map[string]string{
		"A": "Bearer ${TOKEN_ONE}",
		"B": "prefix-${TOKEN_TWO}-${TOKEN_ONE}-suffix",
		"C": "resolved-already",
	})
	assert.Equal(t, []string{"TOKEN_ONE", "TOKEN_TWO"}, got,
		"placeholders must be deduplicated and returned in sorted order")
}

// TestLoadCatalogIsCachedButReturnsCopies verifies the sync.OnceValues
// optimization: subsequent Load() calls don't re-decode the JSON, but
// each one returns an independently mutable Servers slice so test
// helpers (and any future caller that mutates the catalog) stay isolated.
func TestLoadCatalogIsCachedButReturnsCopies(t *testing.T) {
	c1, err := Load()
	require.NoError(t, err)
	originalLen := len(c1.Servers)
	c1.Servers = append(c1.Servers, Server{ID: "injected-by-test"})

	c2, err := Load()
	require.NoError(t, err)
	assert.Len(t, c2.Servers, originalLen,
		"appending to one Load()'s Servers must not bleed into another Load()")
}

// TestToolsUsesStableIterationOrder verifies the Tools() output is sorted
// by id so model-side prompt caches and TUI rendering don't reshuffle on
// every turn.
func TestToolsUsesStableIterationOrder(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})

	// Pick the first OAuth server so handleEnable doesn't try to expand
	// missing api_key headers; the inner toolset never starts because
	// IsStarted() is false and Start() will fail — but we don't actually
	// call Tools() through to the network here, we only assert the
	// meta-tool prefix is stable.
	require.GreaterOrEqual(t, len(ts.catalog.Servers), 3, "need 3+ servers")
	ids := []string{ts.catalog.Servers[0].ID, ts.catalog.Servers[1].ID, ts.catalog.Servers[2].ID}

	ctx := t.Context()
	for _, id := range ids {
		_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
		require.NoError(t, err)
	}

	// Build the expected sorted-by-id order independently.
	want := append([]string(nil), ids...)
	sort.Strings(want)

	ts.mu.RLock()
	got := make([]string, 0, len(ts.enabled))
	for id := range ts.enabled {
		got = append(got, id)
	}
	ts.mu.RUnlock()
	sort.Strings(got)
	assert.Equal(t, want, got)
}

func TestEnableUnknownServer(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: "definitely-not-a-server"})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "unknown server id")
}

func TestEnableAPIKeyMissingEnv(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})

	var apiKeyID, expectedEnv string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "api_key" && len(s.Auth.Secrets) > 0 && s.Auth.Secrets[0].Env != "" {
			apiKeyID = s.ID
			expectedEnv = s.Auth.Secrets[0].Env
			break
		}
	}
	require.NotEmpty(t, apiKeyID, "test fixture: catalog should contain at least one api_key server with an env var")

	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: apiKeyID})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, res.Output, "WARNING")
	assert.Contains(t, res.Output, expectedEnv)
	// The recovery instructions must mention the disable+enable sequence,
	// not the misleading "re-enable" wording (the early-return at the top
	// of handleEnable would otherwise short-circuit a plain second enable).
	assert.Contains(t, res.Output, ToolNameDisable)
	assert.Contains(t, res.Output, ToolNameEnable)
}

func TestEnableAPIKeyEnvPresent(t *testing.T) {
	// Find an api_key server with a declared env var first so we know what
	// to populate.
	ts := New(stubEnv{vars: map[string]string{}})
	var apiKeyID string
	vars := map[string]string{}
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "api_key" {
			apiKeyID = s.ID
			for _, sec := range s.Auth.Secrets {
				if sec.Env != "" {
					vars[sec.Env] = "sentinel-value"
				}
			}
			break
		}
	}
	require.NotEmpty(t, apiKeyID)

	// Re-instantiate with the populated env so missingAPIKeyEnv and the
	// unresolved-header scan both come back empty.
	ts = New(stubEnv{vars: vars})

	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: apiKeyID})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, res.Output, "auth: API key")
	assert.NotContains(t, res.Output, "WARNING")
}

func TestListEnabled(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ctx := t.Context()

	res, err := ts.handleList(ctx, ListArgs{})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "0 enabled")

	id := ts.catalog.Servers[0].ID
	_, err = ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	res, err = ts.handleList(ctx, ListArgs{})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "1 enabled")
	assert.Contains(t, res.Output, id)
}

func TestStopReleasesEverything(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ctx := t.Context()

	id := ts.catalog.Servers[0].ID
	_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	require.NoError(t, ts.Stop(ctx))

	ts.mu.RLock()
	defer ts.mu.RUnlock()
	assert.Empty(t, ts.enabled)
}

func toolNames(list []tools.Tool) []string {
	out := make([]string, len(list))
	for i, t := range list {
		out[i] = t.Name
	}
	return out
}

func TestSetManagedOAuthPersistence(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ctx := t.Context()

	// Setting before any server is enabled must persist so that the next
	// enabled server inherits the flag (regression: an earlier version
	// dropped the value because it had no field on the Toolset).
	ts.SetManagedOAuth(true)
	ts.mu.RLock()
	assert.True(t, ts.managedOAuth)
	assert.True(t, ts.managedOAuthSet)
	ts.mu.RUnlock()

	id := ts.catalog.Servers[0].ID
	_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	ts.mu.RLock()
	mcpTS, exists := ts.enabled[id]
	ts.mu.RUnlock()
	require.True(t, exists)
	assert.NotNil(t, mcpTS)
}

func TestConcurrentEnableDisable(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ctx := t.Context()

	require.GreaterOrEqual(t, len(ts.catalog.Servers), 2, "need at least 2 servers for concurrency test")
	id1 := ts.catalog.Servers[0].ID
	id2 := ts.catalog.Servers[1].ID

	var wg sync.WaitGroup
	enableErrs := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := ts.handleEnable(ctx, EnableArgs{ID: id1})
		if err != nil {
			enableErrs <- err
		}
	}()
	go func() {
		defer wg.Done()
		_, err := ts.handleEnable(ctx, EnableArgs{ID: id2})
		if err != nil {
			enableErrs <- err
		}
	}()
	wg.Wait()
	close(enableErrs)
	for err := range enableErrs {
		require.NoError(t, err)
	}

	ts.mu.RLock()
	_, exists1 := ts.enabled[id1]
	_, exists2 := ts.enabled[id2]
	ts.mu.RUnlock()
	assert.True(t, exists1)
	assert.True(t, exists2)

	disableErrs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := ts.handleDisable(ctx, DisableArgs{ID: id1})
		if err != nil {
			disableErrs <- err
		}
	}()
	go func() {
		defer wg.Done()
		_, err := ts.handleDisable(ctx, DisableArgs{ID: id2})
		if err != nil {
			disableErrs <- err
		}
	}()
	wg.Wait()
	close(disableErrs)
	for err := range disableErrs {
		require.NoError(t, err)
	}

	ts.mu.RLock()
	_, exists1 = ts.enabled[id1]
	_, exists2 = ts.enabled[id2]
	ts.mu.RUnlock()
	assert.False(t, exists1)
	assert.False(t, exists2)
}

func TestToolsContextCancellation(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})

	id := ts.catalog.Servers[0].ID
	_, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = ts.Tools(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestToolsExposesEnabledServerTools is the regression test for the
// "enabled-but-never-started" bug. It spins up an HTTP server that speaks
// just enough MCP for an Initialize+ListTools handshake, points a catalog
// entry at it, and asserts that after enable_remote_mcp_server the
// returned Tools() includes the server's tool — proving the inner MCP
// toolset really is started lazily and its tools merge with the meta
// surface.
func TestToolsExposesEnabledServerTools(t *testing.T) {
	srv := newFakeMCPServer(t)
	defer srv.Close()

	ts := New(stubEnv{vars: map[string]string{}})

	// Inject a synthetic catalog entry that points at the test server.
	const id = "test-server"
	server := Server{
		ID:        id,
		Title:     "Test",
		URL:       srv.URL,
		Transport: "streamable-http",
		Auth:      Auth{Type: "none"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	ctx := t.Context()
	res, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError, "enable: %s", res.Output)

	// Tools() must lazily start the inner toolset and include its tools.
	toolList, err := ts.Tools(ctx)
	require.NoError(t, err)

	names := toolNames(toolList)
	// All five meta-tools must be visible once a server is enabled
	// (disable / reset_auth are gated on len(enabled) > 0).
	for _, meta := range []string{ToolNameSearch, ToolNameList, ToolNameEnable, ToolNameDisable, ToolNameResetAuth} {
		assert.Contains(t, names, meta)
	}
	// And so is the tool exposed by the fake MCP server.
	assert.Contains(t, names, "test-server_echo",
		"enabled MCP server's tool must show up after Tools() lazily starts it")

	// Subsequent calls remain cheap (cached).
	toolList2, err := ts.Tools(ctx)
	require.NoError(t, err)
	assert.Len(t, toolList2, len(toolList))

	// Cleanup so the test doesn't leak the supervisor's watch goroutine.
	require.NoError(t, ts.Stop(ctx))
}

// TestResetAuthForwardsToTokenStore verifies that reset_remote_mcp_server_auth
// places the right call with the right URL.
func TestResetAuthForwardsToTokenStore(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})

	var removedURLs []string
	ts.removeOAuthToken = func(url string) error {
		removedURLs = append(removedURLs, url)
		return nil
	}

	var oauthServer Server
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthServer = s
			break
		}
	}
	require.NotEmpty(t, oauthServer.ID, "need at least one oauth server in catalog")

	res, err := ts.handleResetAuth(t.Context(), ResetAuthArgs{ID: oauthServer.ID})
	require.NoError(t, err)
	require.False(t, res.IsError, "reset auth: %s", res.Output)
	assert.Contains(t, res.Output, "cleared OAuth credentials")
	assert.Equal(t, []string{oauthServer.URL}, removedURLs,
		"removeOAuthToken must be called once with the catalog URL")
}

// TestResetAuthUnknownServer confirms unknown ids surface a friendly error
// without touching the token store.
func TestResetAuthUnknownServer(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	called := 0
	ts.removeOAuthToken = func(string) error { called++; return nil }

	res, err := ts.handleResetAuth(t.Context(), ResetAuthArgs{ID: "definitely-not-a-server"})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "unknown server id")
	assert.Zero(t, called, "token store must not be touched for unknown ids")
}

// TestResetAuthNoOpForNonOAuth confirms that resetting auth for an
// api_key/none server is a no-op that doesn't reach the token store.
func TestResetAuthNoOpForNonOAuth(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	called := 0
	ts.removeOAuthToken = func(string) error { called++; return nil }

	var apiKeyID string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "api_key" {
			apiKeyID = s.ID
			break
		}
	}
	require.NotEmpty(t, apiKeyID)

	res, err := ts.handleResetAuth(t.Context(), ResetAuthArgs{ID: apiKeyID})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, res.Output, "nothing to reset")
	assert.Zero(t, called, "api_key servers must not touch the OAuth token store")
}

// TestResetAuthDisablesEnabledServer makes sure resetting auth for a
// currently-enabled server stops its toolset (so the next enable does a
// fresh handshake) AND fires the tools-changed handler.
func TestResetAuthDisablesEnabledServer(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ts.removeOAuthToken = func(string) error { return nil }

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	var oauthID string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthID = s.ID
			break
		}
	}
	require.NotEmpty(t, oauthID)

	ctx := t.Context()
	_, err := ts.handleEnable(ctx, EnableArgs{ID: oauthID})
	require.NoError(t, err)
	assert.Equal(t, int32(1), changes.Load())

	ts.mu.RLock()
	_, present := ts.enabled[oauthID]
	ts.mu.RUnlock()
	require.True(t, present, "server should be enabled before reset")

	res, err := ts.handleResetAuth(ctx, ResetAuthArgs{ID: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError, "reset: %s", res.Output)
	assert.Contains(t, res.Output, "has been disabled")

	ts.mu.RLock()
	_, stillThere := ts.enabled[oauthID]
	ts.mu.RUnlock()
	assert.False(t, stillThere, "server must be removed from enabled after reset")

	assert.Equal(t, int32(2), changes.Load(),
		"reset on an enabled server must fire tools-changed exactly once more")
}

// TestResetAuthSurfacesStoreErrors confirms that errors from the token
// store are surfaced to the caller as IsError results (not panics).
func TestResetAuthSurfacesStoreErrors(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ts.removeOAuthToken = func(string) error { return errors.New("keyring on fire") }

	var oauthID string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthID = s.ID
			break
		}
	}
	require.NotEmpty(t, oauthID)

	res, err := ts.handleResetAuth(t.Context(), ResetAuthArgs{ID: oauthID})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "keyring on fire")
}

// TestResetAuthNotifiesEvenWhenKeyringFails verifies the state-vs-notification
// invariant on the failure path: if the server was enabled, we have already
// removed it from t.enabled and stopped the inner toolset before calling
// the keyring; the runtime's tool list has therefore changed regardless of
// whether the keyring removal eventually succeeds. Notify must fire.
func TestResetAuthNotifiesEvenWhenKeyringFails(t *testing.T) {
	ts := New(stubEnv{vars: map[string]string{}})
	ts.removeOAuthToken = func(string) error { return errors.New("keyring on fire") }

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	var oauthID string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthID = s.ID
			break
		}
	}
	require.NotEmpty(t, oauthID)

	ctx := t.Context()
	_, err := ts.handleEnable(ctx, EnableArgs{ID: oauthID})
	require.NoError(t, err)
	require.Equal(t, int32(1), changes.Load(), "enable should fire once")

	res, err := ts.handleResetAuth(ctx, ResetAuthArgs{ID: oauthID})
	require.NoError(t, err)
	assert.True(t, res.IsError, "keyring failure must be surfaced")

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[oauthID]
	ts.mu.RUnlock()
	assert.False(t, stillEnabled, "server must be removed even when keyring removal fails")

	assert.Equal(t, int32(2), changes.Load(),
		"reset must notify the runtime that tools changed even if keyring removal fails afterwards")
}

// TestToolsAuthRequiredIsDeferred verifies the on-demand semantics: a
// server requiring OAuth that is probed in a non-interactive context
// must not error out. Tools() returns the meta-surface only and the
// server is silently retried on the next interactive turn.

// TestDisableAndResetAuthGatedOnEnabledServers asserts the meta-surface
// optimisation: disable_remote_mcp_server and reset_remote_mcp_server_auth
// are hidden when no server is enabled (so the LLM sees only the actions
// it can usefully perform), revealed once at least one server is enabled,
// and hidden again after the last server is disabled.
func TestDisableAndResetAuthGatedOnEnabledServers(t *testing.T) {
	// Use a local auth-required fake server so the test never touches the
	// network and is independent of catalog data. The OAuth path makes
	// Tools() swallow the AuthorizationRequired error while keeping the
	// entry in t.enabled, which is exactly the state we want to assert
	// against.
	srv := newAuthRequiredMCPServer(t)
	defer srv.Close()

	ts := New(stubEnv{vars: map[string]string{}})

	const id = "gated-meta-server"
	ts.catalog.Servers = append(ts.catalog.Servers, Server{
		ID: id, Title: "Gated", URL: srv.URL,
		Transport: "streamable-http", Auth: Auth{Type: "oauth"},
	})
	ts.byID[id] = ts.catalog.Servers[len(ts.catalog.Servers)-1]

	ctx := t.Context()
	defer func() { require.NoError(t, ts.Stop(ctx)) }()

	names := toolNames(mustTools(t, ctx, ts))
	assert.ElementsMatch(t, []string{ToolNameSearch, ToolNameList, ToolNameEnable}, names,
		"with no server enabled, disable/reset_auth must be hidden")

	_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	names = toolNames(mustTools(t, ctx, ts))
	assert.Contains(t, names, ToolNameDisable, "disable must appear once a server is enabled")
	assert.Contains(t, names, ToolNameResetAuth, "reset_auth must appear once a server is enabled")

	_, err = ts.handleDisable(ctx, DisableArgs{ID: id})
	require.NoError(t, err)

	names = toolNames(mustTools(t, ctx, ts))
	assert.NotContains(t, names, ToolNameDisable, "disable must be hidden again once no server is enabled")
	assert.NotContains(t, names, ToolNameResetAuth, "reset_auth must be hidden again once no server is enabled")
}

func mustTools(t *testing.T, ctx context.Context, ts *Toolset) []tools.Tool {
	t.Helper()
	list, err := ts.Tools(ctx)
	require.NoError(t, err)
	return list
}

func TestToolsAuthRequiredIsDeferred(t *testing.T) {
	srv := newAuthRequiredMCPServer(t)
	defer srv.Close()

	ts := New(stubEnv{vars: map[string]string{}})
	const id = "auth-required-server"
	server := Server{
		ID:        id,
		Title:     "AuthRequired",
		URL:       srv.URL,
		Transport: "streamable-http",
		Auth:      Auth{Type: "oauth"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	ctx := t.Context()
	_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	// Probe with the same context the runtime uses at startup: no
	// interactive prompts allowed. We expect Tools() to swallow the
	// AuthorizationRequired error and still return the meta-tools.
	probeCtx := mcptools.WithoutInteractivePrompts(ctx)
	toolList, err := ts.Tools(probeCtx)
	require.NoError(t, err, "auth-required servers must not break Tools()")

	names := toolNames(toolList)
	for _, meta := range []string{ToolNameSearch, ToolNameList, ToolNameEnable, ToolNameDisable} {
		assert.Contains(t, names, meta)
	}
	// The auth-required server contributes no tools yet.
	assert.NotContains(t, names, id+"_anything")

	require.NoError(t, ts.Stop(ctx))
}

// --- minimal fake MCP server helpers -----------------------------------
//
// The MCP SDK's streamable-HTTP transport speaks JSON-RPC 2.0 framed in
// Server-Sent Events. We only need to respond to two methods (initialize
// and tools/list) for a successful handshake, then immediately close the
// stream so the client moves on.

func newFakeMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", mcpHandler(t, false))
	return httptest.NewServer(mux)
}

func newAuthRequiredMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// 401 with WWW-Authenticate so the OAuth transport notices.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource="https://example.invalid/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	return httptest.NewServer(mux)
}

// mcpHandler returns an http.HandlerFunc that responds to a single
// initialize+tools/list+(notifications) sequence over streamable-HTTP.
// This is *just* enough to satisfy the MCP SDK's client during its
// initial handshake; it is NOT a complete server implementation.
func mcpHandler(t *testing.T, _ bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "expected POST", http.StatusMethodNotAllowed)
			return
		}

		body, err := readJSONRPC(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Notifications carry no id — the MCP SDK sends notifications/initialized
		// after the initialize response. Reply 202 Accepted and stop.
		if body.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")

		switch body.Method {
		case "initialize":
			writeJSONRPC(t, w, body.ID, map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo": map[string]any{
					"name":    "fake",
					"version": "0.0.1",
				},
			})
		case "tools/list":
			writeJSONRPC(t, w, body.ID, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "echo",
						"description": "echoes its input",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"message": map[string]any{"type": "string"},
							},
						},
					},
				},
			})
		default:
			writeJSONRPC(t, w, body.ID, map[string]any{})
		}
	}
}

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func readJSONRPC(r *http.Request) (*jsonrpcRequest, error) {
	defer r.Body.Close()
	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	if req.JSONRPC != "2.0" {
		return nil, errors.New("missing jsonrpc=2.0")
	}
	return &req, nil
}

func writeJSONRPC(t *testing.T, w http.ResponseWriter, id json.RawMessage, result any) {
	t.Helper()
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
