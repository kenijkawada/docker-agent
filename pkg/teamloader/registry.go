package teamloader

import (
	"cmp"
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/a2a"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/api"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tools/builtin/lsp"
	"github.com/docker/docker-agent/pkg/tools/builtin/memory"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
	"github.com/docker/docker-agent/pkg/tools/builtin/openapi"
	builtinrag "github.com/docker/docker-agent/pkg/tools/builtin/rag"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/tasks"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/builtin/userprompt"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

// ToolsetCreator is a function that creates a toolset based on the provided configuration.
// configName identifies the agent config file (e.g. "memory_agent" from "memory_agent.yaml").
type ToolsetCreator func(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error)

// ToolsetRegistry manages the registration of toolset creators by type.
type ToolsetRegistry interface {
	CreateTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error)
}

func NewDefaultToolsetRegistry() ToolsetRegistry {
	return &toolsetRegistry{
		creators: map[string]ToolsetCreator{
			"todo": func(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return todo.CreateToolSet(toolset)
			},
			"tasks": func(_ context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return tasks.CreateToolSet(toolset, parentDir, runConfig)
			},
			"memory": func(_ context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
				return memory.CreateToolSet(toolset, parentDir, runConfig, configName)
			},
			"think": func(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return think.CreateToolSet()
			},
			"shell": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return shell.CreateToolSet(ctx, toolset, runConfig)
			},
			"script": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return shell.CreateScriptToolSet(ctx, toolset, runConfig)
			},
			"filesystem": func(_ context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return filesystem.CreateToolSet(toolset, runConfig)
			},
			"fetch": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return fetch.CreateToolSet(ctx, toolset, runConfig)
			},
			"mcp": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return mcp.CreateToolSet(ctx, toolset, runConfig)
			},
			"api": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return api.CreateToolSet(ctx, toolset, runConfig)
			},
			"a2a": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return a2a.CreateToolSet(ctx, toolset, runConfig)
			},
			"lsp": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return lsp.CreateToolSet(ctx, toolset, runConfig)
			},
			"user_prompt": func(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return userprompt.CreateToolSet()
			},
			"openapi": func(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return openapi.CreateToolSet(ctx, toolset, runConfig)
			},
			"model_picker": func(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return modelpicker.CreateToolSet(toolset)
			},
			"background_agents": func(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return agenttool.CreateToolSet()
			},
			"rag": func(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
				return builtinrag.CreateToolSet(ctx, toolset, parentDir, runConfig)
			},
		},
	}
}

// toolsetRegistry manages the registration of toolset creators by type.
type toolsetRegistry struct {
	creators map[string]ToolsetCreator
}

// CreateTool creates a toolset using the registered creator for the given type.
//
// Every successful toolset is decorated with tools.WithName so status
// surfaces (the /tools dialog, error messages, …) always have a stable
// user-facing label. The decoration is a no-op for toolsets that
// already advertise a non-empty Name(): it only fills the gap left by
// built-in toolsets that don't take a `name:` field in YAML, replacing
// the previous fallback to fmt.Sprintf("%T", ts).
func (r *toolsetRegistry) CreateTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error) {
	creator, ok := r.creators[toolset.Type]
	if !ok {
		return nil, fmt.Errorf("unknown toolset type: %s", toolset.Type)
	}
	ts, err := creator(ctx, toolset, parentDir, runConfig, agentName)
	if err != nil {
		return nil, err
	}
	return tools.WithName(ts, cmp.Or(toolset.Name, toolset.Type)), nil
}

