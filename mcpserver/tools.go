package mcpserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dagger/container-use/environment"
	"github.com/dagger/container-use/rules"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func validateName(name string) error {
	if name == "" {
		return errors.New("name cannot be empty")
	}

	if strings.Contains(name, " ") {
		return errors.New("name cannot contain spaces, use hyphens (-) instead")
	}

	if strings.Contains(name, "_") {
		return errors.New("name cannot contain underscores, use hyphens (-) instead")
	}

	invalidChars := []string{"~", "^", ":", "?", "*", "[", "\\", "/", "\"", "<", ">", "|", "@", "{", "}", "..", "\t", "\n", "\r"}
	for _, char := range invalidChars {
		if strings.Contains(name, char) {
			return fmt.Errorf("name cannot contain '%s'", char)
		}
	}

	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") ||
		strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return errors.New("name cannot start or end with hyphen or dot")
	}

	if strings.HasSuffix(name, ".lock") {
		return errors.New("name cannot end with '.lock'")
	}

	if len(name) > 100 {
		return errors.New("name cannot exceed 244 bytes")
	}

	return nil
}

type Tool struct {
	Definition mcp.Tool
	Handler    server.ToolHandlerFunc
}

func RunStdioServer(ctx context.Context) error {
	s := server.NewMCPServer(
		"Dagger",
		"1.0.0",
		server.WithInstructions(rules.AgentRules),
	)

	for _, t := range tools {
		s.AddTool(t.Definition, t.Handler)
	}

	slog.Info("starting server")
	return server.ServeStdio(s)
}

var tools = []*Tool{}

func registerTool(tool ...*Tool) {
	for _, t := range tool {
		tools = append(tools, wrapTool(t))
	}
}

func wrapTool(t *Tool) *Tool {
	return &Tool{
		Definition: t.Definition,
		Handler: func(ctx context.Context, request mcp.CallToolRequest) (_ *mcp.CallToolResult, rerr error) {
			slog.Info("Calling tool", "tool", t.Definition.Name)
			defer func() {
				slog.Info("Tool call completed", "tool", t.Definition.Name, "err", rerr)
			}()
			return t.Handler(ctx, request)
		},
	}
}

func init() {
	registerTool(
		EnvironmentOpenTool,
		EnvironmentUpdateTool,

		// EnvironmentListTool,
		// EnvironmentHistoryTool,
		// EnvironmentRevertTool,
		// EnvironmentForkTool,

		EnvironmentRunCmdTool,
		// EnvironmentSetEnvTool,

		// EnvironmentUploadTool,
		// EnvironmentDownloadTool,
		// EnvironmentDiffTool,

		EnvironmentFileReadTool,
		EnvironmentFileListTool,
		EnvironmentFileWriteTool,
		EnvironmentFileDeleteTool,
		// EnvironmentRevisionDiffTool,

		EnvironmentCheckpointTool,
	)
}

type EnvironmentResponse struct {
	ID              string   `json:"id"`
	BaseImage       string   `json:"base_image"`
	SetupCommands   []string `json:"setup_commands"`
	Instructions    string   `json:"instructions"`
	Workdir         string   `json:"workdir"`
	Branch          string   `json:"branch"`
	TrackingBranch  string   `json:"tracking_branch"`
	CheckoutCommand string   `json:"checkout_command_for_human"`
}

func EnvironmentToCallResult(env *environment.Environment) (*mcp.CallToolResult, error) {
	resp := &EnvironmentResponse{
		ID:              env.ID,
		Instructions:    env.Instructions,
		BaseImage:       env.BaseImage,
		SetupCommands:   env.SetupCommands,
		Workdir:         env.Workdir,
		Branch:          env.ID,
		TrackingBranch:  fmt.Sprintf("container-use/%s", env.ID),
		CheckoutCommand: fmt.Sprintf("git checkout %s", env.ID),
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to marshal response", err), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

var EnvironmentOpenTool = &Tool{
	Definition: mcp.NewTool("environment_open",
		mcp.WithDescription(`Opens (or creates) a development environment.
The environment is the result of a the setups commands on top of the base image.
Read carefully the instructions to understand the environment.
DO NOT manually install toolchains inside the environment, instead explicitly call environment_update`,
		),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being opened or created."),
		),
		mcp.WithString("source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
		),
		mcp.WithString("name",
			mcp.Description("Name of the environment. Use hyphens (-) to separate words, no spaces or underscores allowed (e.g., 'my-web-app' not 'my web app' or 'my_web_app')"),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		source, err := request.RequireString("source")
		if err != nil {
			return nil, err
		}
		name, err := request.RequireString("name")
		if err != nil {
			return nil, err
		}
		if err := validateName(name); err != nil {
			return mcp.NewToolResultErrorFromErr("invalid name", err), nil
		}
		// FIXME(aluzzardi): This should call `environment.Open` instead of `environment.Create` but it's currently broken
		env, err := environment.Create(ctx, request.GetString("explanation", ""), source, name)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to open environment", err), nil
		}
		return EnvironmentToCallResult(env)
	},
}

var EnvironmentUpdateTool = &Tool{
	Definition: mcp.NewTool("environment_update",
		mcp.WithDescription("Updates an environment with new instructions and toolchains."+
			"If the environment is missing any tools or instructions, you MUST call this function to update the environment."+
			"You MUST update the environment with any useful information or tools. You will be resumed with no other context than the information provided here"),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being updated."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment to update."),
			mcp.Required(),
		),
		mcp.WithString("instructions",
			mcp.Description("The instructions for the environment. This should contain any information that might be useful to operate in the environment, such as what tools are available, what commands to use to build/test/etc"),
			mcp.Required(),
		),

		mcp.WithString("base_image",
			mcp.Description("Change the base image for the environment."),
			mcp.Required(),
		),
		mcp.WithArray("setup_commands",
			mcp.Description("Commands that will be executed on top of the base image to set up the environment. Similar to `RUN` instructions in Dockerfiles."),
			mcp.Required(),
			mcp.Items(map[string]any{"type": "string"}),
		),
		mcp.WithArray("secrets",
			mcp.Description(`Secret references in the format of "SECRET_NAME=schema://value

Secrets will be available in the environment as environment variables ($SECRET_NAME).

Supported schemas are:
- file://PATH: local file path
- env://NAME: environment variable
- op://<vault-name>/<item-name>/[section-name/]<field-name>: 1Password secret
`),
			mcp.Required(),
			mcp.Items(map[string]any{"type": "string"}),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}
		instructions, err := request.RequireString("instructions")
		if err != nil {
			return nil, err
		}
		baseImage, err := request.RequireString("base_image")
		if err != nil {
			return nil, err
		}
		setupCommands, err := request.RequireStringSlice("setup_commands")
		if err != nil {
			return nil, err
		}
		secrets, err := request.RequireStringSlice("secrets")
		if err != nil {
			return nil, err
		}

		if err := env.Update(ctx, request.GetString("explanation", ""), instructions, baseImage, setupCommands, secrets); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to update environment", err), nil
		}
		return EnvironmentToCallResult(env)
	},
}

var EnvironmentListTool = &Tool{
	Definition: mcp.NewTool("environment_list",
		mcp.WithDescription("List available environments"),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being listed."),
		),
		mcp.WithString("source",
			mcp.Description("The source directory of the environment."), //  This can be a local folder (e.g. file://) or a URL to a git repository (e.g. https://github.com/user/repo.git, git@github.com:user/repo.git)"),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		source, err := request.RequireString("source")
		if err != nil {
			return nil, err
		}
		envs, err := environment.List(ctx, source)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("invalid source", err), nil
		}
		out, err := json.Marshal(envs)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(out)), nil
	},
}

var EnvironmentForkTool = &Tool{
	Definition: mcp.NewTool("environment_fork",
		mcp.WithDescription("Create a new environment from an existing environment."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being forked."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment to fork."),
			mcp.Required(),
		),
		mcp.WithNumber("version",
			mcp.Description("Version of the environment to fork. Defaults to latest version."),
		),
		mcp.WithString("name",
			mcp.Description("Name of the new environment. Use hyphens (-) to separate words, no spaces or underscores allowed (e.g., 'my-forked-app' not 'my forked app' or 'my_forked_app')"),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}

		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		name, err := request.RequireString("name")
		if err != nil {
			return nil, err
		}
		if err := validateName(name); err != nil {
			return mcp.NewToolResultErrorFromErr("invalid name", err), nil
		}

		var version *environment.Version
		if v, ok := request.GetArguments()["version"].(environment.Version); ok {
			version = &v
		}

		fork, err := env.Fork(ctx, request.GetString("explanation", ""), name, version)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to fork environment", err), nil
		}

		return mcp.NewToolResultText("environment forked successfully into ID " + fork.ID), nil
	},
}

var EnvironmentHistoryTool = &Tool{
	Definition: mcp.NewTool("environment_history",
		mcp.WithDescription("List the history of an environment."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being listed."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}

		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		history := env.History
		out, err := json.Marshal(history)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(out)), nil
	},
}

var EnvironmentRevertTool = &Tool{
	Definition: mcp.NewTool("environment_revert",
		mcp.WithDescription("Revert the environment to a specific version."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being listed."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithNumber("version",
			mcp.Description("The version to revert to."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}

		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		version, err := request.RequireInt("version")
		if err != nil {
			return nil, err
		}

		if err := env.Revert(ctx, request.GetString("explanation", ""), environment.Version(version)); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to revert environment", err), nil
		}

		return mcp.NewToolResultText("environment reverted successfully"), nil
	},
}

var EnvironmentRunCmdTool = &Tool{
	Definition: mcp.NewTool("environment_run_cmd",
		mcp.WithDescription("Run a command on behalf of the user in the terminal."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this command is being run."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("command",
			mcp.Description("The terminal command to execute. If empty, the environment's default command is used."),
		),
		mcp.WithString("shell",
			mcp.Description("The shell that will be interpreting this command (default: sh)"),
		),
		mcp.WithBoolean("background",
			mcp.Description(`Run the command in the background
Must ALWAYS be set for long running command (e.g. http server).
Failure to do so will result in the tool being stuck, awaiting for the command to finish.`,
			),
		),
		mcp.WithBoolean("use_entrypoint",
			mcp.Description("Use the image entrypoint, if present, by prepending it to the args."),
		),
		mcp.WithArray("ports",
			mcp.Description("Ports to expose. Only works with background environments. For each port, returns the internal (for use by other environments) and external (for use by the user) address."),
			mcp.Items(map[string]any{"type": "number"}),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}
		command := request.GetString("command", "")
		shell := request.GetString("shell", "sh")

		background := request.GetBool("background", false)
		if background {
			ports := []int{}
			if portList, ok := request.GetArguments()["ports"].([]any); ok {
				for _, port := range portList {
					ports = append(ports, int(port.(float64)))
				}
			}
			endpoints, err := env.RunBackground(ctx, request.GetString("explanation", ""), command, shell, ports, request.GetBool("use_entrypoint", false))
			if err != nil {
				return mcp.NewToolResultErrorFromErr("failed to run command", err), nil
			}

			out, err := json.Marshal(endpoints)
			if err != nil {
				return nil, err
			}

			return mcp.NewToolResultText(fmt.Sprintf(`Command started in the background. Endpoints are %s

Any changes to the container workdir (%s) WILL NOT be committed to container-use/%s

Background commands are unaffected by filesystem and any other kind of changes. You need to start a new command for changes to take effect.`,
				string(out), env.Workdir, env.ID)), nil
		}

		stdout, err := env.Run(ctx, request.GetString("explanation", ""), command, shell, request.GetBool("use_entrypoint", false))
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to run command", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("%s\n\nAny changes to the container workdir (%s) have been committed and pushed to container-use/%s", stdout, env.Workdir, env.ID)), nil
	},
}

var EnvironmentSetEnvTool = &Tool{
	Definition: mcp.NewTool("environment_set_env",
		mcp.WithDescription("Set environment variables for an environment."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why these environment variables are being set."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithArray("envs",
			mcp.Description("The environment variables to set."),
			mcp.Items(map[string]any{"type": "string"}),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}
		envs, err := request.RequireStringSlice("envs")
		if err != nil {
			return nil, err
		}
		if err := env.SetEnv(ctx, request.GetString("explanation", ""), envs); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to set environment variables", err), nil
		}
		return mcp.NewToolResultText("environment variables set successfully"), nil
	},
}

var EnvironmentUploadTool = &Tool{
	Definition: mcp.NewTool("environment_upload",
		mcp.WithDescription("Upload files to an environment."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this file is being uploaded."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("source",
			mcp.Description("The source directory to be uploaded to the environment. This can be a local folder (e.g. file://) or a URL to a git repository (e.g. https://github.com/user/repo.git, git@github.com:user/repo.git)"),
			mcp.Required(),
		),
		mcp.WithString("target",
			mcp.Description("The target destination in the environment where to upload files."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		source, err := request.RequireString("source")
		if err != nil {
			return nil, err
		}
		target, err := request.RequireString("target")
		if err != nil {
			return nil, err
		}

		if err := env.Upload(ctx, request.GetString("explanation", ""), source, target); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to upload files", err), nil
		}

		return mcp.NewToolResultText("files uploaded successfully"), nil
	},
}

var EnvironmentDownloadTool = &Tool{
	Definition: mcp.NewTool("environment_download",
		mcp.WithDescription("Download files from an environment to the local filesystem."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this file is being downloaded."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("source",
			mcp.Description("The source directory to be downloaded from the environment."),
			mcp.Required(),
		),
		mcp.WithString("target",
			mcp.Description("The target destination on the local filesystem where to download files."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		source, err := request.RequireString("source")
		if err != nil {
			return nil, err
		}
		target, err := request.RequireString("target")
		if err != nil {
			return nil, errors.New("target must be a string")
		}

		if err := env.Download(ctx, source, target); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to download files", err), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("files downloaded successfully to %s", target)), nil
	},
}

var EnvironmentDiffTool = &Tool{
	Definition: mcp.NewTool("environment_remote_diff",
		mcp.WithDescription("Diff files between an environment and the local filesystem or git repository."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this diff is being run."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("source",
			mcp.Description("The source directory to be compared. This can be a local folder (e.g. file://) or a URL to a git repository (e.g. https://github.com/user/repo.git, git@github.com:user/repo.git)"),
			mcp.Required(),
		),
		mcp.WithString("target",
			mcp.Description("The target destination on the environment filesystem where to compare against."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		source, err := request.RequireString("source")
		if err != nil {
			return nil, err
		}
		target, err := request.RequireString("target")
		if err != nil {
			return nil, errors.New("target must be a string")
		}

		diff, err := env.RemoteDiff(ctx, source, target)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to diff", err), nil
		}

		return mcp.NewToolResultText(diff), nil
	},
}

var EnvironmentFileReadTool = &Tool{
	Definition: mcp.NewTool("environment_file_read",
		mcp.WithDescription("Read the contents of a file, specifying a line range or the entire file."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this file is being read."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("target_file",
			mcp.Description("Path of the file to read, absolute or relative to the workdir"),
			mcp.Required(),
		),
		mcp.WithBoolean("should_read_entire_file",
			mcp.Description("Whether to read the entire file. Defaults to false."),
		),
		mcp.WithNumber("start_line_one_indexed",
			mcp.Description("The one-indexed line number to start reading from (inclusive)."),
		),
		mcp.WithNumber("end_line_one_indexed_inclusive",
			mcp.Description("The one-indexed line number to end reading at (inclusive)."),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		targetFile, err := request.RequireString("target_file")
		if err != nil {
			return nil, err
		}
		shouldReadEntireFile := request.GetBool("should_read_entire_file", false)
		startLineOneIndexed := request.GetInt("start_line_one_indexed", 0)
		endLineOneIndexedInclusive := request.GetInt("end_line_one_indexed_inclusive", 0)

		fileContents, err := env.FileRead(ctx, targetFile, shouldReadEntireFile, startLineOneIndexed, endLineOneIndexedInclusive)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to read file", err), nil
		}

		return mcp.NewToolResultText(fileContents), nil
	},
}

var EnvironmentFileListTool = &Tool{
	Definition: mcp.NewTool("environment_file_list",
		mcp.WithDescription("List the contents of a directory"),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this directory is being listed."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("path",
			mcp.Description("Path of the directory to list contents of, absolute or relative to the workdir"),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		path, err := request.RequireString("path")
		if err != nil {
			return nil, err
		}

		out, err := env.FileList(ctx, path)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to list directory", err), nil
		}

		return mcp.NewToolResultText(out), nil
	},
}

var EnvironmentFileWriteTool = &Tool{
	Definition: mcp.NewTool("environment_file_write",
		mcp.WithDescription("Write the contents of a file."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this file is being written."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("target_file",
			mcp.Description("Path of the file to write, absolute or relative to the workdir."),
			mcp.Required(),
		),
		mcp.WithString("contents",
			mcp.Description("Full text content of the file you want to write."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		targetFile, err := request.RequireString("target_file")
		if err != nil {
			return nil, err
		}
		contents, err := request.RequireString("contents")
		if err != nil {
			return nil, err
		}

		if err := env.FileWrite(ctx, request.GetString("explanation", ""), targetFile, contents); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to write file", err), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("file %s written successfully, changes pushed to container-use/%s", targetFile, env.ID)), nil
	},
}

var EnvironmentFileDeleteTool = &Tool{
	Definition: mcp.NewTool("environment_file_delete",
		mcp.WithDescription("Deletes a file at the specified path."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this file is being deleted."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("target_file",
			mcp.Description("Path of the file to delete, absolute or relative to the workdir."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		targetFile, err := request.RequireString("target_file")
		if err != nil {
			return nil, err
		}

		if err := env.FileDelete(ctx, request.GetString("explanation", ""), targetFile); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to delete file", err), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("file %s deleted successfully, changes pushed to container-use/%s", targetFile, env.ID)), nil
	},
}

var EnvironmentRevisionDiffTool = &Tool{
	Definition: mcp.NewTool("environment_revision_diff",
		mcp.WithDescription("Diff files between multiple revisions of an environment."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this diff is being run."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("path",
			mcp.Description("The path within the environment to be diffed. Defaults to workdir."),
		),
		mcp.WithNumber("from_version",
			mcp.Description("Compute the diff starting from this version"),
			mcp.Required(),
		),
		mcp.WithNumber("to_version",
			mcp.Description("Compute the diff ending at this version. Defaults to latest version."),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		path := request.GetString("path", "")
		fromVersion, err := request.RequireInt("from_version")
		if err != nil {
			return nil, err
		}
		toVersion := request.GetInt("to_version", int(env.History.LatestVersion()))

		diff, err := env.RevisionDiff(ctx, path, environment.Version(fromVersion), environment.Version(toVersion))
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to diff", err), nil
		}

		return mcp.NewToolResultText(diff), nil
	},
}

var EnvironmentCheckpointTool = &Tool{
	Definition: mcp.NewTool("environment_checkpoint",
		mcp.WithDescription("Checkpoints an environment in its current state as a container."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this checkpoint is being created."),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("destination",
			mcp.Description("Container image destination to checkpoint to (e.g. registry.com/user/image:tag"),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := request.RequireString("environment_id")
		if err != nil {
			return nil, err
		}
		env := environment.Get(envID)
		if env == nil {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}
		destination, err := request.RequireString("destination")
		if err != nil {
			return nil, err
		}

		endpoint, err := env.Checkpoint(ctx, destination)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to checkpoint", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Checkpoint pushed to %q. You MUST use the full content addressed (@sha256:...) reference in `docker` commands. The entrypoint is set to `sh`, keep that in mind when giving commands to the container.", endpoint)), nil
	},
}
