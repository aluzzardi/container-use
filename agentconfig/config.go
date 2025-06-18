package agentconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dagger/container-use/rules"
	"github.com/mitchellh/go-homedir"
	"gopkg.in/yaml.v3"
)

// Configure sets up an agent to use container-use
func Configure(a *Agent, root string, confirm func(string) (bool, error)) error {
	// Install MCP server
	if err := installMCP(a, confirm); err != nil {
		return err
	}

	// Install rules
	if err := installRules(a, root, confirm); err != nil {
		return err
	}

	return nil
}
func installMCP(a *Agent, confirm func(string) (bool, error)) error {
	ok, err := confirm(fmt.Sprintf("Install container-use MCP server in %s?", a.Name))
	if err != nil {
		return fmt.Errorf("failed to get confirmation: %w", err)
	}
	if !ok {
		fmt.Printf("👉 Skipping: MCP installation (user declined)\n")
		return nil
	}

	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get absolute path of current executable: %w", err)
	}

	if err := a.ConfigureMCP(bin); err != nil {
		return fmt.Errorf("failed to configure MCP: %w", err)
	}
	return nil
}

func installRules(a *Agent, root string, confirm func(string) (bool, error)) error {
	// Some agents don't have rules
	if a.RulesFile == "" {
		return nil
	}

	rulesFile := path.Join(root, a.RulesFile)

	// Get confirmation
	ok, err := confirm(fmt.Sprintf("Install container-use rules to %s?", rulesFile))
	if err != nil {
		return fmt.Errorf("failed to get confirmation: %w", err)
	}
	if !ok {
		fmt.Printf("👉 Skipping: Rules installation (user declined)\n")
		return nil
	}

	// Create rules directory
	if err := os.MkdirAll(filepath.Dir(rulesFile), 0755); err != nil {
		return fmt.Errorf("failed to create rules directory: %w", err)
	}

	// Write rules file
	content := "# Environment\n" + rules.AgentRules

	switch a.RuleStrategy {
	case RuleStrategyReplace:
		if err := os.WriteFile(rulesFile, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write rules: %w", err)
		}
	case RuleStrategyMerge:
		// Merge rules file
		// Read existing rules
		existing, err := os.ReadFile(rulesFile)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to read existing rules: %w", err)
		}

		// Look for section markers
		const marker = "<!-- container-use-rules -->"
		existingStr := string(existing)

		if strings.Contains(existingStr, marker) {
			// Update existing section
			parts := strings.Split(existingStr, marker)
			if len(parts) != 3 {
				return fmt.Errorf("malformed rules file - expected single section marked with %s", marker)
			}
			newContent := parts[0] + marker + "\n" + content + "\n" + marker + parts[2]
			if err := os.WriteFile(rulesFile, []byte(newContent), 0644); err != nil {
				return fmt.Errorf("failed to update rules: %w", err)
			}
		} else {
			// Append new section
			newContent := string(existing)
			if len(newContent) > 0 && !strings.HasSuffix(newContent, "\n") {
				newContent += "\n"
			}
			newContent += "\n" + marker + "\n" + content + "\n" + marker + "\n"
			if err := os.WriteFile(rulesFile, []byte(newContent), 0644); err != nil {
				return fmt.Errorf("failed to append rules: %w", err)
			}
		}
	}

	fmt.Printf("✨ Installed rules to %s\n", rulesFile)
	return nil
}

func resolvePath(root, path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	path = strings.ReplaceAll(path, "$HOME", home)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func loadConfig(configFile string, format string) (map[string]any, error) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]any), nil
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var config map[string]any

	switch format {
	case "yaml":
		if err := yaml.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("failed to parse yaml: %w", err)
		}
	case "json":
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("failed to parse json: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported config format: %s", format)
	}

	return config, nil
}

func updateConfig(configFile string, newConfig map[string]any, format string) error {
	var err error
	configFile, err = homedir.Expand(configFile)
	if err != nil {
		return fmt.Errorf("failed to expand home directory: %w", err)
	}

	currentConfig, err := loadConfig(configFile, format)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Deep merge the configs
	merged := mergeConfigs(currentConfig, newConfig)

	// Ensure config directory exists
	if err := os.MkdirAll(filepath.Dir(configFile), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Save merged config
	var data []byte

	switch format {
	case "yaml":
		data, err = yaml.Marshal(merged)
	case "json":
		data, err = json.MarshalIndent(merged, "", "  ")
	}
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(configFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	fmt.Printf("✨ Updated configuration in %s\n", configFile)
	return nil
}

func mergeConfigs(base, new any) any {
	switch newVal := new.(type) {
	case map[string]any:
		if baseVal, ok := base.(map[string]any); ok {
			result := make(map[string]any)
			// Copy base values
			for k, v := range baseVal {
				result[k] = v
			}
			// Merge new values
			for k, v := range newVal {
				if existing, exists := result[k]; exists {
					result[k] = mergeConfigs(existing, v)
				} else {
					result[k] = v
				}
			}
			return result
		}
	case []any:
		if baseVal, ok := base.([]any); ok {
			// For arrays, just append new values
			return append(baseVal, newVal...)
		}
	}
	// For non-maps/arrays or type mismatches, use the new value
	return new
}
