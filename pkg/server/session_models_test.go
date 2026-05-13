package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// modelSwitchingRuntime is a fakeRuntime variant that supports model
// switching so the /models and /model endpoints can be exercised
// without spinning up a real LocalRuntime.
type modelSwitchingRuntime struct {
	fakeRuntime

	mu              sync.Mutex
	currentAgent    string
	availableModels []runtime.ModelChoice
	overrides       map[string]string
	setErr          error
}

func newModelSwitchingRuntime(models []runtime.ModelChoice) *modelSwitchingRuntime {
	return &modelSwitchingRuntime{
		currentAgent:    "root",
		availableModels: models,
		overrides:       make(map[string]string),
	}
}

func (m *modelSwitchingRuntime) CurrentAgentName() string { return m.currentAgent }

func (m *modelSwitchingRuntime) SupportsModelSwitching() bool { return true }

func (m *modelSwitchingRuntime) AvailableModels(_ context.Context) []runtime.ModelChoice {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]runtime.ModelChoice, len(m.availableModels))
	copy(out, m.availableModels)
	return out
}

func (m *modelSwitchingRuntime) SetAgentModel(_ context.Context, agentName, modelRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	if modelRef == "" {
		delete(m.overrides, agentName)
		return nil
	}
	m.overrides[agentName] = modelRef
	return nil
}

// startAttachedServer wires a SessionManager + HTTP server backed by an
// in-process listener and registers a t.Cleanup that closes the listener
// (and unblocks the Serve goroutine) when the test finishes.
func startAttachedServer(t *testing.T, ctx context.Context, sm *SessionManager) string {
	t.Helper()
	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ctx, ln) }()
	return "http://" + ln.Addr().String()
}

func TestSessionManager_CreateSession_KeepsModelOverrides(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	template := &session.Session{
		AgentModelOverrides: map[string]string{
			"root":       "openai/gpt-4o",
			"researcher": "anthropic/claude-sonnet-4-0",
		},
		CustomModelsUsed: []string{"openai/gpt-4o"},
	}

	created, err := sm.CreateSession(ctx, template)
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)

	assert.Equal(t, "openai/gpt-4o", created.AgentModelOverrides["root"])
	assert.Equal(t, "anthropic/claude-sonnet-4-0", created.AgentModelOverrides["researcher"])
	assert.Equal(t, []string{"openai/gpt-4o"}, created.CustomModelsUsed)

	// Mutating the template after creation must not affect the stored session.
	template.AgentModelOverrides["root"] = "mutated"
	assert.Equal(t, "openai/gpt-4o", created.AgentModelOverrides["root"])

	stored, err := store.GetSession(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-4o", stored.AgentModelOverrides["root"])
}

func TestAttachedServer_GetSessionModels(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.AgentModelOverrides = map[string]string{"root": "openai/gpt-4o"}
	require.NoError(t, store.AddSession(ctx, sess))

	choices := []runtime.ModelChoice{
		{Name: "default", Ref: "openai/gpt-4o-mini", Provider: "openai", Model: "gpt-4o-mini", IsDefault: true},
		{Name: "custom", Ref: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o"},
	}
	fake := newModelSwitchingRuntime(choices)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", nil)

	var got runtime.SessionModelsResponse
	require.NoError(t, json.Unmarshal(resp, &got))

	assert.Equal(t, "root", got.Agent)
	assert.Equal(t, "openai/gpt-4o", got.CurrentModelRef)
	require.Len(t, got.Models, 2)
	assert.Equal(t, "openai/gpt-4o-mini", got.Models[0].Ref)
	assert.True(t, got.Models[0].IsDefault)
	assert.False(t, got.Models[0].IsCurrent, "default must not be marked current when an override is active")
	assert.Equal(t, "openai/gpt-4o", got.Models[1].Ref)
	assert.True(t, got.Models[1].IsCurrent, "the model matching the override must be marked current")
}

// When no override is set, the agent's default model must be marked
// IsCurrent so the picker can highlight it without a second round trip.
func TestAttachedServer_GetSessionModels_DefaultMarkedCurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	choices := []runtime.ModelChoice{
		{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
		{Name: "other", Ref: "openai/gpt-4o"},
	}
	fake := newModelSwitchingRuntime(choices)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", nil)

	var got runtime.SessionModelsResponse
	require.NoError(t, json.Unmarshal(resp, &got))

	assert.Empty(t, got.CurrentModelRef)
	require.Len(t, got.Models, 2)
	assert.True(t, got.Models[0].IsCurrent, "default model must be marked current when no override is set")
	assert.False(t, got.Models[1].IsCurrent)
}

// Custom (provider/model) refs from the session history must be appended
// to the picker so a user can pick a previously used model again.
func TestAttachedServer_GetSessionModels_AppendsCustomModels(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.CustomModelsUsed = []string{"openai/gpt-4o"}
	require.NoError(t, store.AddSession(ctx, sess))

	choices := []runtime.ModelChoice{
		{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
	}
	fake := newModelSwitchingRuntime(choices)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", nil)

	var got runtime.SessionModelsResponse
	require.NoError(t, json.Unmarshal(resp, &got))

	require.Len(t, got.Models, 2)
	assert.Equal(t, "openai/gpt-4o-mini", got.Models[0].Ref)
	assert.Equal(t, "openai/gpt-4o", got.Models[1].Ref)
	assert.True(t, got.Models[1].IsCustom)
}

func TestAttachedServer_SetSessionModel_PersistsOverride(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	resp := httpDoTCP(t, ctx, http.MethodPatch, addr+"/api/sessions/"+sess.ID+"/model",
		api.SetSessionModelRequest{Model: "anthropic/claude-sonnet-4-0"})

	var got api.SetSessionModelResponse
	require.NoError(t, json.Unmarshal(resp, &got))
	assert.Equal(t, "root", got.Agent)
	assert.Equal(t, "anthropic/claude-sonnet-4-0", got.Model)

	// The runtime must have received the override.
	fake.mu.Lock()
	assert.Equal(t, "anthropic/claude-sonnet-4-0", fake.overrides["root"])
	fake.mu.Unlock()

	// The session in the store must reflect the override and track the
	// custom model for future picks.
	stored, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude-sonnet-4-0", stored.AgentModelOverrides["root"])
	assert.Contains(t, stored.CustomModelsUsed, "anthropic/claude-sonnet-4-0")
}

func TestAttachedServer_SetSessionModel_EmptyClearsOverride(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.AgentModelOverrides = map[string]string{"root": "openai/gpt-4o"}
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	_ = httpDoTCP(t, ctx, http.MethodPatch, addr+"/api/sessions/"+sess.ID+"/model",
		api.SetSessionModelRequest{Model: ""})

	stored, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	_, exists := stored.AgentModelOverrides["root"]
	assert.False(t, exists, "override should be cleared")
}

func TestAttachedServer_SetSessionModel_PostVerbAlsoWorks(t *testing.T) {
	// The pre-existing pkg/runtime Client.SetAgentModel POSTs to
	// /api/sessions/:id/model. The server must accept POST as well as
	// PATCH so RemoteRuntime keeps working without a coordinated bump.
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	_ = httpDoTCP(t, ctx, http.MethodPost, addr+"/api/sessions/"+sess.ID+"/model",
		api.SetSessionModelRequest{Model: "openai/gpt-4o"})

	fake.mu.Lock()
	assert.Equal(t, "openai/gpt-4o", fake.overrides["root"])
	fake.mu.Unlock()
}

func TestAttachedServer_GetSessionModels_NotSupported(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	addr := startAttachedServer(t, ctx, sm)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

// failingStore wraps an in-memory store so UpdateSession can be made to
// fail on demand to exercise the rollback path of SetSessionAgentModel.
type failingStore struct {
	session.Store

	mu         sync.Mutex
	failUpdate bool
}

func (s *failingStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	s.mu.Lock()
	fail := s.failUpdate
	s.mu.Unlock()
	if fail {
		return errors.New("synthetic store failure")
	}
	return s.Store.UpdateSession(ctx, sess)
}

// When the session store rejects the persistence write, the in-memory
// session and the runtime override must both be rolled back so the next
// read does not surface state that was never persisted.
func TestSessionManager_SetSessionAgentModel_RollsBackOnStoreFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := &failingStore{Store: session.NewInMemorySessionStore()}
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	store.mu.Lock()
	store.failUpdate = true
	store.mu.Unlock()

	_, _, err := sm.SetSessionAgentModel(ctx, sess.ID, "openai/gpt-4o")
	require.Error(t, err)

	// In-memory session must not contain the override.
	_, exists := sess.AgentModelOverrides["root"]
	assert.False(t, exists, "in-memory override must be rolled back")
	assert.NotContains(t, sess.CustomModelsUsed, "openai/gpt-4o", "CustomModelsUsed must be rolled back")

	// Runtime must not have the override either.
	fake.mu.Lock()
	_, runtimeHas := fake.overrides["root"]
	fake.mu.Unlock()
	assert.False(t, runtimeHas, "runtime override must be rolled back")
}

// decorateModelChoices is exercised through the GET handler tests above;
// this unit test pins a few important corner cases that are too tedious
// to reach over HTTP.
func TestDecorateModelChoices(t *testing.T) {
	t.Parallel()

	t.Run("synthesizes choice for inline override not in list", func(t *testing.T) {
		t.Parallel()
		got := decorateModelChoices(
			[]runtime.ModelChoice{{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true}},
			"anthropic/claude-sonnet-4-0",
			nil,
		)
		require.Len(t, got, 2)
		assert.Equal(t, "anthropic/claude-sonnet-4-0", got[1].Ref)
		assert.Equal(t, "anthropic", got[1].Provider)
		assert.Equal(t, "claude-sonnet-4-0", got[1].Model)
		assert.True(t, got[1].IsCurrent)
		assert.True(t, got[1].IsCustom)
	})

	t.Run("does not duplicate custom ref already in list", func(t *testing.T) {
		t.Parallel()
		got := decorateModelChoices(
			[]runtime.ModelChoice{{Name: "default", Ref: "openai/gpt-4o", IsDefault: true}},
			"",
			[]string{"openai/gpt-4o"},
		)
		require.Len(t, got, 1)
		assert.Equal(t, "openai/gpt-4o", got[0].Ref)
	})

	t.Run("non-provider override (config key) does not synthesize choice", func(t *testing.T) {
		t.Parallel()
		// "my_model" is a config key (no slash); when not in the runtime's
		// list we should NOT fabricate a choice for it because we have no
		// provider/model breakdown to display.
		got := decorateModelChoices(
			[]runtime.ModelChoice{{Name: "default", Ref: "default", IsDefault: true}},
			"my_model",
			nil,
		)
		require.Len(t, got, 1)
		assert.False(t, got[0].IsCurrent, "default must not be marked current when override is unknown")
	})
}
