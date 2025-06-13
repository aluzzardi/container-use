package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"dagger.io/dagger"
	"github.com/dagger/container-use/environment"
	"github.com/dagger/container-use/environment/remotes"
	"github.com/dagger/container-use/mcpserver"
	"github.com/spf13/cobra"
)

var dag *dagger.Client

func dumpStacks() {
	buf := make([]byte, 1<<20) // 1MB buffer
	n := runtime.Stack(buf, true)
	io.MultiWriter(logWriter, os.Stderr).Write(buf[:n])
}

var (
	rootCmd = &cobra.Command{
		Use:   "cu",
		Short: "Container Use",
		Long:  `MCP server to add container superpowers to your AI agent.`,
	}

	stdioCmd = &cobra.Command{
		Use:   "stdio",
		Short: "Start stdio server",
		Long:  `Start a server that communicates via standard input/output streams using JSON-RPC messages.`,
		RunE: func(app *cobra.Command, _ []string) error {
			ctx := app.Context()

			slog.Info("connecting to dagger")

			var err error
			dag, err = dagger.Connect(ctx, dagger.WithLogOutput(logWriter))
			if err != nil {
				slog.Error("Error starting dagger", "error", err)
				os.Exit(1)
			}
			defer dag.Close()

			localRemote := remotes.NewLocalRemote(dag)
			environment.Initialize(dag, localRemote)
			return mcpserver.RunStdioServer(ctx)
		},
	}
)

func init() {
	rootCmd.AddCommand(
		stdioCmd,
		terminalCmd,
	)
}

func handleSIGUSR(sigusrCh <-chan os.Signal) {
	for sig := range sigusrCh {
		if sig == syscall.SIGUSR1 {
			dumpStacks()
		}
	}
}

func main() {
	sigusrCh := make(chan os.Signal, 1)
	signal.Notify(sigusrCh, syscall.SIGUSR1)

	go handleSIGUSR(sigusrCh)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := setupLogger(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup logger: %v\n", err)
		os.Exit(1)
	}

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
