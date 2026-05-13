// Package mcpcatalog exposes the Docker MCP Catalog's remote
// streamable-http servers as a single agent-side toolset that supports
// on-demand activation.
//
// The toolset surfaces four meta-tools to the model:
//
//   - search_remote_mcp_servers — case-insensitive fuzzy search over the
//     curated catalog (id / title / description / category / tags).
//   - list_remote_mcp_servers   — show currently enabled servers.
//   - enable_remote_mcp_server  — instantiate an *mcp.Toolset for a server
//     (defers the actual TCP connect / OAuth handshake until Tools() is
//     next enumerated).
//   - disable_remote_mcp_server — stop the toolset and remove its tools.
//
// Activated servers' tools are merged into Tools(); tool list changes are
// reported via a tools.ChangeNotifier handler so the runtime refreshes
// the LLM's tool catalogue as soon as a server is enabled or disabled.
//
// On-demand semantics: the expensive parts — DNS, TCP, MCP handshake,
// OAuth flow — happen the first time Tools() is called for a freshly
// enabled server. The handshake runs through the same lifecycle.Supervisor
// the YAML-declared `mcp.remote` toolset uses, so OAuth elicitation and
// tool-list-change notifications behave identically.
package mcpcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

const (
	ToolNameSearch  = "search_remote_mcp_servers"
	ToolNameEnable  = "enable_remote_mcp_server"
	ToolNameDisable = "disable_remote_mcp_server"
	ToolNameList    = "list_remote_mcp_servers"
)

// Toolset implements on-demand activation of remote (streamable-http) MCP
// servers from the Docker MCP Catalog.
type Toolset struct {
	catalog  *Catalog
	byID     map[string]Server
	expander *js.Expander
	env      environment.Provider

	mu sync.RWMutex
	// enabled holds the per-server StartableToolSet wrapper. Wrapping the
	// inner *mcp.Toolset in a StartableToolSet gives us:
	//   - single-flight, idempotent Start() (so Tools() can call it on
	//     every enumeration without re-running the MCP handshake);
	//   - de-duplicated Start failure warnings (once per failure streak,
	//     reset by a subsequent success);
	//   - the same lifecycle wrapper the agent uses for YAML-declared
	//     toolsets, so the inner mcp.Toolset is treated identically.
	enabled map[string]*tools.StartableToolSet

	// elicitationHandler / oauthSuccessHandler / managedOAuth /
	// toolsChangedHandler are captured before any server is enabled
	// (the runtime calls these via tools.As[...] from
	// configureToolsetHandlers at the start of every turn). They are
	// re-applied to each new mcp.Toolset on enable so OAuth elicitation,
	// OAuth-success refreshes, the managed-vs-unmanaged flag and
	// tool-list change notifications behave identically to a YAML-
	// declared `mcp.remote` toolset.
	elicitationHandler  tools.ElicitationHandler
	oauthSuccessHandler func()
	toolsChangedHandler func()
	managedOAuth        bool
	managedOAuthSet     bool // distinguishes "default" from "explicitly false"
}

var (
	_ tools.ToolSet        = (*Toolset)(nil)
	_ tools.Startable      = (*Toolset)(nil)
	_ tools.Instructable   = (*Toolset)(nil)
	_ tools.Describer      = (*Toolset)(nil)
	_ tools.ChangeNotifier = (*Toolset)(nil)
	_ tools.Elicitable     = (*Toolset)(nil)
	_ tools.OAuthCapable   = (*Toolset)(nil)
)

// New returns a Toolset backed by the embedded catalog. envProvider is used
// to resolve ${ENV_VAR} placeholders in catalog headers (e.g. the Apify
// `Authorization: Bearer ${APIFY_API_KEY}` header) at enable time, mirroring
// how a YAML-declared `mcp.remote` toolset works.
func New(envProvider environment.Provider) *Toolset {
	cat := MustLoad()
	byID := make(map[string]Server, len(cat.Servers))
	for _, s := range cat.Servers {
		byID[s.ID] = s
	}
	return &Toolset{
		catalog:  cat,
		byID:     byID,
		expander: js.NewJsExpander(envProvider),
		env:      envProvider,
		enabled:  make(map[string]*tools.StartableToolSet),
	}
}

// Describe returns a short, user-visible label for the /tools dialog.
func (t *Toolset) Describe() string {
	return fmt.Sprintf("mcp_catalog(remote streamable-http, %d servers)", t.catalog.Count)
}

// Instructions tell the model how to discover and activate servers.
func (t *Toolset) Instructions() string {
	return `## Remote MCP Catalog

You have access to a curated catalog of remote MCP servers (Docker MCP
Catalog, streamable-http only). They are NOT active by default.

Workflow:
  1. Call ` + ToolNameSearch + ` with a keyword to discover matching servers.
     Use any term related to the user's intent ("notion", "stripe",
     "docs", "search", "browser", …).
  2. Call ` + ToolNameEnable + ` with the server's "id" to activate it.
     This adds the server's tools to your set on the *next* turn.
     Authentication (OAuth or API key) is deferred — it is triggered when
     the host enumerates the server's tools, which happens once enabling
     completes. For api_key servers, make sure the listed env var(s) are
     set in the user's shell BEFORE enabling, otherwise the server will
     refuse the connection.
  3. Use the newly activated tools as you would any other.
  4. Call ` + ToolNameDisable + ` to remove a server when no longer needed.

Prefer enabling only the servers you actually need — every server adds
tools to the prompt and contributes to context usage.`
}

// Start is a no-op: the catalog is embedded and no servers are auto-enabled.
// Lifecycle for individual MCP toolsets is managed when Enable / Disable
// are invoked, with first-use lazy start happening inside Tools().
func (t *Toolset) Start(context.Context) error { return nil }

// Stop tears down every enabled MCP toolset. Errors are logged but do not
// abort the loop so a misbehaving server can't block agent shutdown.
func (t *Toolset) Stop(ctx context.Context) error {
	t.mu.Lock()
	enabled := t.enabled
	t.enabled = make(map[string]*tools.StartableToolSet)
	t.mu.Unlock()

	for id, ts := range enabled {
		if err := ts.Stop(ctx); err != nil {
			slog.WarnContext(ctx, "Failed to stop remote MCP toolset", "id", id, "error", err)
		}
	}
	return nil
}

// SetElicitationHandler is captured here and re-attached to every freshly
// activated MCP toolset so OAuth flows can prompt the user.
func (t *Toolset) SetElicitationHandler(handler tools.ElicitationHandler) {
	t.mu.Lock()
	t.elicitationHandler = handler
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if e, ok := tools.As[tools.Elicitable](ts); ok {
			e.SetElicitationHandler(handler)
		}
	}
}

// SetOAuthSuccessHandler is captured here and re-attached to every freshly
// activated MCP toolset so the runtime refreshes its tool list once OAuth
// completes.
func (t *Toolset) SetOAuthSuccessHandler(handler func()) {
	t.mu.Lock()
	t.oauthSuccessHandler = handler
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if o, ok := tools.As[tools.OAuthCapable](ts); ok {
			o.SetOAuthSuccessHandler(handler)
		}
	}
}

// SetManagedOAuth forwards the managed-OAuth flag to every enabled
// toolset; new toolsets pick it up at enable time.
func (t *Toolset) SetManagedOAuth(managed bool) {
	t.mu.Lock()
	t.managedOAuth = managed
	t.managedOAuthSet = true
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if o, ok := tools.As[tools.OAuthCapable](ts); ok {
			o.SetManagedOAuth(managed)
		}
	}
}

// SetToolsChangedHandler is invoked by the runtime to be notified when
// the set of available tools changes. We forward to the activated MCP
// toolsets *and* call it ourselves on every Enable / Disable so the
// runtime sees the meta-tool surface change too.
func (t *Toolset) SetToolsChangedHandler(handler func()) {
	t.mu.Lock()
	t.toolsChangedHandler = handler
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if n, ok := tools.As[tools.ChangeNotifier](ts); ok {
			n.SetToolsChangedHandler(handler)
		}
	}
}

// snapshotEnabled returns the currently enabled toolsets as a fresh slice.
// Caller MUST hold t.mu (read or write). Used to forward setter calls
// outside the critical section.
func (t *Toolset) snapshotEnabled() []*tools.StartableToolSet {
	out := make([]*tools.StartableToolSet, 0, len(t.enabled))
	for _, ts := range t.enabled {
		out = append(out, ts)
	}
	return out
}

// Tools returns the meta-tools plus every tool exposed by an activated
// remote MCP server. Tools from unactivated servers are intentionally
// hidden so they don't bloat the prompt.
//
// First-call lazy start: each enabled server is Start()'d on its first
// enumeration. On startup the runtime probes tools with a non-interactive
// context (mcp.WithoutInteractivePrompts), so OAuth-pending servers fail
// fast with mcp.IsAuthorizationRequired and are silently deferred. On
// interactive turns, Start() blocks on OAuth elicitation as the user
// expects, and the resulting tools join the result set on the next
// enumeration.
func (t *Toolset) Tools(ctx context.Context) ([]tools.Tool, error) {
	result := []tools.Tool{
		{
			Name:         ToolNameSearch,
			Category:     "mcp_catalog",
			Description:  "Search the Docker MCP Catalog for remote streamable-http MCP servers matching a keyword. Returns id, title, description, auth requirements and category for each hit.",
			Parameters:   tools.MustSchemaFor[SearchArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleSearch),
			Annotations: tools.ToolAnnotations{
				Title:        "Search remote MCP servers",
				ReadOnlyHint: true,
			},
		},
		{
			Name:         ToolNameList,
			Category:     "mcp_catalog",
			Description:  "List currently enabled remote MCP servers and their connection state.",
			Parameters:   tools.MustSchemaFor[ListArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleList),
			Annotations: tools.ToolAnnotations{
				Title:        "List enabled remote MCP servers",
				ReadOnlyHint: true,
			},
		},
		{
			Name:         ToolNameEnable,
			Category:     "mcp_catalog",
			Description:  "Activate a remote MCP server from the catalog by id. Connection (and any required OAuth flow or API-key check) is deferred until the host next lists the agent's tools.",
			Parameters:   tools.MustSchemaFor[EnableArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleEnable),
			Annotations: tools.ToolAnnotations{
				Title: "Enable remote MCP server",
			},
		},
		{
			Name:         ToolNameDisable,
			Category:     "mcp_catalog",
			Description:  "Disable a previously enabled remote MCP server, dropping its tools from the active set.",
			Parameters:   tools.MustSchemaFor[DisableArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleDisable),
			Annotations: tools.ToolAnnotations{
				Title: "Disable remote MCP server",
			},
		},
	}

	t.mu.RLock()
	enabled := make([]enabledServer, 0, len(t.enabled))
	for id, ts := range t.enabled {
		enabled = append(enabled, enabledServer{id: id, ts: ts})
	}
	t.mu.RUnlock()

	for _, e := range enabled {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !e.ts.IsStarted() {
			if err := e.ts.Start(ctx); err != nil {
				// Auth-required is an *expected* deferral when probing
				// with a non-interactive context (startup tool count) or
				// when the elicitation bridge is not yet ready. Silent
				// — the next interactive turn will retry and surface
				// the OAuth dialog naturally.
				if mcp.IsAuthorizationRequired(err) {
					slog.DebugContext(ctx, "Remote MCP server requires authorization; deferred to next turn",
						"id", e.id)
					continue
				}
				// Real failure: log once per streak (StartableToolSet
				// dedupes) so a misbehaving server doesn't flood logs.
				if e.ts.ShouldReportFailure() {
					slog.WarnContext(ctx, "Failed to start enabled remote MCP server",
						"id", e.id, "error", err)
				} else {
					slog.DebugContext(ctx, "Remote MCP server still unavailable",
						"id", e.id, "error", err)
				}
				continue
			}
		}

		serverTools, err := e.ts.Tools(ctx)
		if err != nil {
			slog.WarnContext(ctx, "Failed to list tools for enabled remote MCP server",
				"id", e.id, "error", err)
			continue
		}
		result = append(result, serverTools...)
	}

	return result, nil
}

// enabledServer pairs an id with its toolset for stable iteration outside
// the lock. It exists so callers can correlate "the server that failed
// to start" with its catalog id without re-reading the map.
type enabledServer struct {
	id string
	ts *tools.StartableToolSet
}

// SearchArgs is the input schema for the search meta-tool.
type SearchArgs struct {
	// Query is the keyword to look for. Empty matches everything.
	Query string `json:"query" jsonschema:"Search keyword (matches id, title, description, category and tags; case-insensitive). Leave empty to list every catalog server."`
}

// SearchResult is one row in the search response.
type SearchResult struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Auth        string   `json:"auth"`
	URL         string   `json:"url"`
	Enabled     bool     `json:"enabled"`
}

func (t *Toolset) handleSearch(_ context.Context, args SearchArgs) (*tools.ToolCallResult, error) {
	q := strings.ToLower(strings.TrimSpace(args.Query))

	t.mu.RLock()
	defer t.mu.RUnlock()

	matches := make([]SearchResult, 0)
	for _, s := range t.catalog.Servers {
		if q != "" && !matchesQuery(s, q) {
			continue
		}
		_, isEnabled := t.enabled[s.ID]
		matches = append(matches, SearchResult{
			ID:          s.ID,
			Title:       s.Title,
			Description: s.Description,
			Category:    s.Category,
			Tags:        s.Tags,
			Auth:        s.Auth.Type,
			URL:         s.URL,
			Enabled:     isEnabled,
		})
	}

	if len(matches) == 0 {
		return tools.ResultError(fmt.Sprintf("no remote MCP servers match %q (catalog has %d entries)", args.Query, t.catalog.Count)), nil
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })

	out, err := json.Marshal(matches)
	if err != nil {
		return nil, err
	}
	return tools.ResultSuccess(fmt.Sprintf("found %d server(s):\n%s", len(matches), string(out))), nil
}

// matchesQuery returns true if any of the searchable string fields contains q.
// q is expected to be already lower-cased and trimmed.
func matchesQuery(s Server, q string) bool {
	for _, field := range []string{s.ID, s.Title, s.Description, s.Category} {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	for _, tag := range s.Tags {
		if strings.Contains(strings.ToLower(tag), q) {
			return true
		}
	}
	return false
}

// EnableArgs is the input schema for enable_remote_mcp_server.
type EnableArgs struct {
	ID string `json:"id" jsonschema:"Catalog id of the server to enable (use search_remote_mcp_servers to find it)."`
}

func (t *Toolset) handleEnable(ctx context.Context, args EnableArgs) (*tools.ToolCallResult, error) {
	id := strings.TrimSpace(args.ID)
	server, ok := t.byID[id]
	if !ok {
		return tools.ResultError(fmt.Sprintf("unknown server id %q (use %s first to discover available ids)", id, ToolNameSearch)), nil
	}

	t.mu.Lock()
	if _, exists := t.enabled[id]; exists {
		t.mu.Unlock()
		return tools.ResultSuccess(fmt.Sprintf("server %q is already enabled", id)), nil
	}

	// Pre-flight: warn (don't block) if an api_key server is missing its env var.
	// We do not block because the user may set the variable later, or rely on
	// the model to surface the error from the first tool call.
	missing := t.missingAPIKeyEnv(ctx, server)

	headers := t.expander.ExpandMap(ctx, server.Headers)
	mcpToolset := mcp.NewRemoteToolset(id, server.URL, server.Transport, headers, nil)

	// Re-attach the captured handlers so OAuth flows behave identically to
	// a YAML-declared mcp.remote toolset. Apply BEFORE wrapping so we hit
	// the *mcp.Toolset's typed setters directly without a tools.As walk.
	if t.elicitationHandler != nil {
		mcpToolset.SetElicitationHandler(t.elicitationHandler)
	}
	if t.oauthSuccessHandler != nil {
		mcpToolset.SetOAuthSuccessHandler(t.oauthSuccessHandler)
	}
	if t.toolsChangedHandler != nil {
		mcpToolset.SetToolsChangedHandler(t.toolsChangedHandler)
	}
	if t.managedOAuthSet {
		mcpToolset.SetManagedOAuth(t.managedOAuth)
	}

	wrapped := tools.NewStartable(mcpToolset)
	t.enabled[id] = wrapped
	notify := t.toolsChangedHandler
	t.mu.Unlock()

	// Notify the runtime that the meta-tool surface itself changed.
	if notify != nil {
		notify()
	}

	msg := strings.Builder{}
	fmt.Fprintf(&msg, "enabled %q (%s) — connection deferred until the host next enumerates tools.\n", id, server.Title)
	fmt.Fprintf(&msg, "endpoint: %s\n", server.URL)
	switch server.Auth.Type {
	case "oauth":
		msg.WriteString("auth: OAuth — an authorization URL will be elicited on the next turn.\n")
	case "api_key":
		if len(missing) > 0 {
			fmt.Fprintf(&msg, "auth: API key — WARNING: the following env vars are NOT set: %s. Set them and re-enable, otherwise tool calls will fail.\n", strings.Join(missing, ", "))
		} else {
			msg.WriteString("auth: API key — env vars present, ready to use.\n")
		}
	default:
		msg.WriteString("auth: none — ready to use.\n")
	}
	return tools.ResultSuccess(msg.String()), nil
}

// missingAPIKeyEnv returns the names of api_key env vars that are not
// available from the toolset's env provider. Empty result means "all good".
// Returns nil for non api_key servers.
func (t *Toolset) missingAPIKeyEnv(ctx context.Context, s Server) []string {
	if s.Auth.Type != "api_key" || t.env == nil {
		return nil
	}
	var missing []string
	for _, sec := range s.Auth.Secrets {
		if sec.Env == "" {
			continue
		}
		if v, ok := t.env.Get(ctx, sec.Env); !ok || v == "" {
			missing = append(missing, sec.Env)
		}
	}
	return missing
}

// DisableArgs is the input schema for disable_remote_mcp_server.
type DisableArgs struct {
	ID string `json:"id" jsonschema:"Catalog id of the server to disable."`
}

func (t *Toolset) handleDisable(ctx context.Context, args DisableArgs) (*tools.ToolCallResult, error) {
	id := strings.TrimSpace(args.ID)

	t.mu.Lock()
	wrapped, exists := t.enabled[id]
	if !exists {
		t.mu.Unlock()
		return tools.ResultError(fmt.Sprintf("server %q is not enabled", id)), nil
	}
	delete(t.enabled, id)
	notify := t.toolsChangedHandler
	t.mu.Unlock()

	if err := wrapped.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// Stop failures aren't fatal — the entry is already gone from
		// t.enabled. Just log and tell the model the server is off.
		slog.WarnContext(ctx, "Failed to stop remote MCP toolset on disable", "id", id, "error", err)
	}

	if notify != nil {
		notify()
	}

	return tools.ResultSuccess(fmt.Sprintf("disabled %q", id)), nil
}

// ListArgs is the input schema for list_remote_mcp_servers (no params).
type ListArgs struct{}

// EnabledServer reports the runtime state of a single enabled MCP server.
type EnabledServer struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Auth    string `json:"auth"`
	Started bool   `json:"started"`
}

func (t *Toolset) handleList(_ context.Context, _ ListArgs) (*tools.ToolCallResult, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	enabled := make([]EnabledServer, 0, len(t.enabled))
	for id, ts := range t.enabled {
		s := t.byID[id]
		enabled = append(enabled, EnabledServer{
			ID:      id,
			Title:   s.Title,
			URL:     s.URL,
			Auth:    s.Auth.Type,
			Started: ts.IsStarted(),
		})
	}
	sort.Slice(enabled, func(i, j int) bool { return enabled[i].ID < enabled[j].ID })

	out, err := json.Marshal(enabled)
	if err != nil {
		return nil, err
	}
	return tools.ResultSuccess(fmt.Sprintf("%d enabled server(s):\n%s", len(enabled), string(out))), nil
}
