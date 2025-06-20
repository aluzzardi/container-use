package agentconfig

import (
	"strings"
)

type RuleStrategy int

const (
	RuleStrategyMerge RuleStrategy = iota
	RuleStrategyReplace
)

// Agent defines how to configure a coding agent to use container-use
type Agent struct {
	// Name of the agent (e.g., "goose", "claude")
	Name string

	// Detect returns true if this agent is being used in the current project
	Detect func(root string) bool

	// ConfigureMCP returns the configuration to add/merge into the agent's config
	ConfigureMCP func(cmd string) error

	// RulesFile is where to install agent-specific instructions
	RulesFile string

	// RuleStrategy defines how to handle the rules file (e.g., "replace", "merge")
	RuleStrategy RuleStrategy
}

type Agents []*Agent

func (agents Agents) Get(name string) *Agent {
	for _, a := range agents {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func (agents Agents) String() string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}

	return strings.Join(names, ", ")
}

// Supported returns all supported agents
func Supported() Agents {
	return Agents{
		gooseAgent,
		claudeAgent,
	}
}
