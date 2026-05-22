package e2e_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/skills"
)

func TestDebug_Toolsets_None(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "toolsets", "testdata/no_tools.yaml")

	require.Equal(t, "No tools for root\n", output)
}

func TestDebug_Toolsets_Todo(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "toolsets", "testdata/todo_tools.yaml")

	require.Equal(t, "2 tool(s) for root:\n + create_todo - Create a new todo item with a description\n + list_todos - List all current todos with their status\n", output)
}

func TestDebug_Skills_None(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "skills", "testdata/no_tools.yaml")

	require.Equal(t, "No skills for root\n", output)
}

// TestDebug_Skills_Local stages two skills (one regular, one forked) in an
// isolated kit directory and asserts that `debug skills` lists each one with
// its name, description, and the [forked] marker for fork-context skills.
func TestDebug_Skills_Local(t *testing.T) {
	kit := t.TempDir()
	writeSkill(t, filepath.Join(kit, skills.KitSkillsSubdir, "plain"),
		"---\nname: plain\ndescription: A plain skill\n---\nbody\n")
	writeSkill(t, filepath.Join(kit, skills.KitSkillsSubdir, "forky"),
		"---\nname: forky\ndescription: A forked skill\ncontext: fork\n---\nbody\n")

	t.Setenv(skills.KitDirEnv, kit)

	output := runCLI(t, "debug", "skills", "testdata/skills_local.yaml")

	require.Equal(t,
		"2 skill(s) for root:\n"+
			" + forky [forked] - A forked skill\n"+
			" + plain - A plain skill\n",
		output,
	)
}

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}
