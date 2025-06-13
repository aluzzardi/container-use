package main

import (
	"fmt"
	"log/slog"
	"os"

	"dagger.io/dagger"
	"github.com/dagger/container-use/environment"
	"github.com/dagger/container-use/environment/remotes"
	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:   "delete <env>",
	Short: "Delete an environment",
	Long:  `Delete an environment and its associated resources.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		envName := args[0]

		dag, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
		if err != nil {
			return fmt.Errorf("failed to connect to dagger: %w", err)
		}
		defer dag.Close()
		localRemote := remotes.NewLocalRemote(dag)
		environment.Initialize(dag, localRemote)

		env := environment.Get(envName)
		if env == nil {
			// Try to open if not in memory
			var openErr error
			slog.Info("Opening environment for deletion", "name", envName)
			env, openErr = environment.Open(ctx, "delete environment", ".", envName)
			if openErr != nil {
				return fmt.Errorf("environment '%s' not found: %w", envName, openErr)
			}
		}

		if err := env.Delete(ctx); err != nil {
			return fmt.Errorf("failed to delete environment: %w", err)
		}

		fmt.Printf("Environment '%s' deleted successfully.\n", envName)
		fmt.Println("To view this change, use: git checkout <branch_name>")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(deleteCmd)
}
