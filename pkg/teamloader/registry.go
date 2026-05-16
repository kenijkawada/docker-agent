package teamloader

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/rag"
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
	"github.com/docker/docker-agent/pkg/tools/workingdir"
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
			"todo":              todo.CreateToolSet,
			"tasks":             tasks.CreateToolSet,
			"memory":            memory.CreateToolSet,
			"think":             think.CreateToolSet,
			"shell":             shell.CreateToolSet,
			"script":            shell.CreateScriptToolSet,
			"filesystem":        filesystem.CreateToolSet,
			"fetch":             fetch.CreateToolSet,
			"mcp":               mcp.CreateToolSet,
			"api":               api.CreateToolSet,
			"a2a":               a2a.CreateToolSet,
			"lsp":               lsp.CreateToolSet,
			"user_prompt":       userprompt.CreateToolSet,
			"openapi":           openapi.CreateToolSet,
			"model_picker":      createModelPickerTool,
			"background_agents": createBackgroundAgentsTool,
			"rag":               createRAGTool,
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

// resolveToolsetWorkingDir returns the effective working directory for a toolset process.
//
// Resolution rules:
//   - If toolsetWorkingDir is empty, agentWorkingDir is returned unchanged.
//   - Shell patterns (~ and ${VAR}/$VAR) are expanded before any further processing.
//   - If the expanded path is absolute, it is returned as-is.
//   - If the expanded path is relative and agentWorkingDir is non-empty,
//     it is joined with agentWorkingDir and made absolute via filepath.Abs.
//   - If the expanded path is relative and agentWorkingDir is empty,
//     the relative path is returned unchanged (caller will inherit the process cwd).
//
// Note: unlike resolveToolsetPath, this helper does not enforce containment
// within the agent working directory. working_dir is treated like command/args —
// a trusted, operator-authored value where cross-tree references (e.g. a sibling
// module root in a monorepo) are intentional and must not be silently blocked.
func resolveToolsetWorkingDir(toolsetWorkingDir, agentWorkingDir string) string {
	return workingdir.Resolve(toolsetWorkingDir, agentWorkingDir)
}

func createModelPickerTool(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if len(toolset.Models) == 0 {
		return nil, errors.New("model_picker toolset requires at least one model")
	}
	return modelpicker.NewModelPickerTool(toolset.Models), nil
}

func createBackgroundAgentsTool(_ context.Context, _ latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	return agenttool.NewToolSet(), nil
}

func createRAGTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if toolset.RAGConfig == nil {
		return nil, errors.New("rag toolset requires rag_config (should have been resolved from ref)")
	}

	ragName := cmp.Or(toolset.Name, "rag")

	mgr, err := rag.NewManager(ctx, ragName, toolset.RAGConfig, rag.ManagersBuildConfig{
		ParentDir:     parentDir,
		ModelsGateway: runConfig.ModelsGateway,
		Env:           runConfig.EnvProvider(),
		Models:        runConfig.Models,
		Providers:     runConfig.Providers,
		RuntimeConfig: runConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create RAG manager: %w", err)
	}

	toolName := cmp.Or(mgr.ToolName(), ragName)
	return builtinrag.NewRAGTool(mgr, toolName), nil
}
