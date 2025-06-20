package agentconfig

import (
	"os"
	"path/filepath"
)

var gooseAgent = &Agent{
	Name: "goose",
	Detect: func(root string) bool {
		if _, err := os.Stat(filepath.Join(root, ".goosehints")); err == nil {
			return true
		}
		return false
	},
	ConfigureMCP: func(cmd string) error {
		return updateConfig(
			"~/.config/goose/config.yaml",
			map[string]any{
				"extensions": map[string]any{
					"container-use": map[string]any{
						"name":    "container-use",
						"type":    "stdio",
						"enabled": true,
						"cmd":     cmd,
						"args":    []string{"stdio"},
						"envs":    map[string]any{},
					},
				},
			},
			"yaml",
		)

		return nil
	},
}
