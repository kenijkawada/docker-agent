package acp

import (
	"context"
	"os"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

// createToolsetRegistry creates a custom toolset registry with ACP-specific filesystem toolset
func createToolsetRegistry(agent *Agent) teamloader.ToolsetRegistry {
	return &acpToolsetRegistry{
		agent:    agent,
		registry: teamloader.NewDefaultToolsetRegistry(),
	}
}

type acpToolsetRegistry struct {
	agent    *Agent
	registry teamloader.ToolsetRegistry
}

func (r *acpToolsetRegistry) CreateTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error) {
	if toolset.Type == "filesystem" {
		wd := runConfig.WorkingDir
		if wd == "" {
			var err error
			wd, err = os.Getwd()
			if err != nil {
				return nil, err
			}
		}

		return NewFilesystemToolset(r.agent, wd), nil
	}

	return r.registry.CreateTool(ctx, toolset, parentDir, runConfig, agentName)
}
