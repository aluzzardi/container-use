package main

import (
	"fmt"

	"github.com/dagger/container-use/agentconfig"
	"github.com/dagger/container-use/repository"
	"github.com/erikgeiser/promptkit/confirmation"
	"github.com/spf13/cobra"
)

var configureCmd = &cobra.Command{
	Use:   "configure",
	Short: "Configure AI agent to use container-use",
	Long: `Configure AI agents to use container-use as an MCP server.

This command will:
1. Add container-use to the agent's configuration
2. Install agent-specific rules and instructions

If no --agent flag is provided, will auto-detect agents in use.
If no agents are detected, you must specify one with --agent.

Examples:
  # Auto-detect and configure agents in use
  cu configure

  # Configure specific agent
  cu configure --agent goose

  # Configure without prompting
  cu configure --yes`,
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := repository.Open(cmd.Context(), ".")
		if err != nil {
			return err
		}

		yes, err := cmd.Flags().GetBool("yes")
		if err != nil {
			return err
		}
		requestedAgent, err := cmd.Flags().GetString("agent")
		if err != nil {
			return err
		}

		var (
			supportedAgents = agentconfig.Supported()
			selectedAgents  agentconfig.Agents
		)

		// If specific agent requested, validate it
		if requestedAgent != "" {
			a := supportedAgents.Get(requestedAgent)
			if a == nil {
				return fmt.Errorf("❌ unsupported agent type: %s (supported: %s)", requestedAgent, supportedAgents.String())
			}
			selectedAgents = agentconfig.Agents{a}
		} else {
			fmt.Println("🔍 Detecting agents...")

			// Detect which agents are in use
			for _, a := range supportedAgents {
				if a.Detect(repo.SourcePath()) {
					selectedAgents = append(selectedAgents, a)
				}
			}
		}

		// If no agents detected and none specified, show error
		if len(selectedAgents) == 0 {
			supported := make([]string, len(supportedAgents))
			for i, a := range supportedAgents {
				supported[i] = a.Name
			}
			return fmt.Errorf("❌ no agents detected - please specify one with --agent (%s)", supportedAgents.String())
		}

		// Function to get user confirmation
		confirm := func(msg string) (bool, error) {
			if yes {
				fmt.Printf("✨ %s? Yes (auto-confirmed)\n", msg)
				return true, nil
			}

			prompt := confirmation.New(fmt.Sprintf("✨ %s", msg), confirmation.Yes)
			result, err := prompt.RunPrompt()
			if err != nil {
				return false, nil // User aborted (Esc/Ctrl+C)
			}
			return result, nil
		}

		var errors []error
		configured := false

		for _, a := range selectedAgents {
			fmt.Printf("🤖 Configuring %s...\n", a.Name)

			if err := agentconfig.Configure(a, repo.SourcePath(), confirm); err != nil {
				errors = append(errors, fmt.Errorf("%s: %w", a.Name, err))
				fmt.Printf("❌ Failed to configure %s\n", a.Name)
			} else {
				configured = true
				fmt.Printf("✅ Successfully configured %s\n", a.Name)
			}
		}

		if len(errors) > 0 {
			fmt.Println("\n❌ Some configurations failed:")
			for _, err := range errors {
				fmt.Printf("  • %v\n", err)
			}
			return fmt.Errorf("failed to configure some agents")
		}

		if !configured {
			return fmt.Errorf("❌ no agents were configured")
		}

		fmt.Println("\n🎉 All configurations completed successfully!")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(configureCmd)
	configureCmd.Flags().StringP("agent", "a", "", "Specific agent to configure (goose, claude)")
	configureCmd.Flags().BoolP("yes", "y", false, "Auto-confirm all changes")
}
