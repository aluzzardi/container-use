package mcpserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/dagger/container-use/environment"
	"github.com/dagger/container-use/repository"
	"github.com/dagger/container-use/rules"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func openRepository(ctx context.Context, request mcp.CallToolRequest) (*repository.Repository, error) {
	source, err := request.RequireString("environment_source")
	if err != nil {
		return nil, err
	}
	repo, err := repository.Open(ctx, source)
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func openEnvironment(ctx context.Context, request mcp.CallToolRequest) (*repository.Repository, *environment.Environment, error) {
	repo, err := openRepository(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	envID, err := request.RequireString("environment_id")
	if err != nil {
		return nil, nil, err
	}
	env, err := repo.Get(ctx, envID)
	if err != nil {
		return nil, nil, err
	}
	return repo, env, nil
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
		EnvironmentCreateTool,
		EnvironmentUpdateTool,

		EnvironmentRunCmdTool,

		EnvironmentFileReadTool,
		EnvironmentFileListTool,
		EnvironmentFileWriteTool,
		EnvironmentFileDeleteTool,

		EnvironmentAddServiceTool,

		EnvironmentCheckpointTool,
	)
}

type EnvironmentResponse struct {
	ID               string                 `json:"id"`
	Description      string                 `json:"short_description"`
	BaseImage        string                 `json:"base_image"`
	SetupCommands    []string               `json:"setup_commands"`
	Instructions     string                 `json:"instructions"`
	Workdir          string                 `json:"workdir"`
	Branch           string                 `json:"branch"`
	TrackingBranch   string                 `json:"tracking_branch"`
	CheckoutCommand  string                 `json:"checkout_command_for_human"`
	HostWorktreePath string                 `json:"host_worktree_path"`
	Services         []*environment.Service `json:"services,omitempty"`
}

func marshalEnvironment(env *environment.Environment) (string, error) {
	resp := &EnvironmentResponse{
		ID:               env.ID,
		Description:      env.State.Description,
		Instructions:     env.Config.Instructions,
		BaseImage:        env.Config.BaseImage,
		SetupCommands:    env.Config.SetupCommands,
		Workdir:          env.Config.Workdir,
		Branch:           env.ID,
		TrackingBranch:   fmt.Sprintf("container-use/%s", env.ID),
		CheckoutCommand:  fmt.Sprintf("git checkout %s", env.ID),
		HostWorktreePath: env.Worktree,
		Services:         env.Services,
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return "", fmt.Errorf("failed to marshal response: %w", err)
	}
	return string(out), nil
}

func EnvironmentToCallResult(env *environment.Environment) (*mcp.CallToolResult, error) {
	out, err := marshalEnvironment(env)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to marshal environment", err), nil
	}
	return mcp.NewToolResultText(out), nil
}

var EnvironmentOpenTool = &Tool{
	Definition: mcp.NewTool("environment_open",
		mcp.WithDescription("Opens an existing environment. Return format is same as environment_create."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being opened."),
		),
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment to open."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
		}
		return EnvironmentToCallResult(env)
	},
}

var EnvironmentCreateTool = &Tool{
	Definition: mcp.NewTool("environment_create",
		mcp.WithDescription(`Creates a new development environment.
The environment is the result of a the setups commands on top of the base image.
Read carefully the instructions to understand the environment.
DO NOT manually install toolchains inside the environment, instead explicitly call environment_update`,
		),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being created."),
		),
		mcp.WithString("description",
			mcp.Description("Short description of the work that is happening in this environment. Keep this description updated using `environment_update`."),
			mcp.Required(),
		),
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repo, err := openRepository(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the repository", err), nil
		}
		description, err := request.RequireString("description")
		if err != nil {
			return nil, err
		}

		env, err := repo.Create(ctx, description, request.GetString("explanation", ""))
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to create environment", err), nil
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
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment to update."),
			mcp.Required(),
		),
		mcp.WithString("instructions",
			mcp.Description("The instructions for the environment. This should contain any information that might be useful to operate in the environment, such as what tools are available, what commands to use to build/test/etc"),
			mcp.Required(),
		),
		mcp.WithString("description",
			mcp.Description("Short description of the work that is happening in this environment.."),
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
		mcp.WithArray("envs",
			mcp.Description("The environment variables to set (e.g. `[\"FOO=bar\", \"BAZ=qux\"]`)."),
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
		repo, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
		}

		config := env.Config.Copy()

		instructions, err := request.RequireString("instructions")
		if err != nil {
			return nil, err
		}
		config.Instructions = instructions

		baseImage, err := request.RequireString("base_image")
		if err != nil {
			return nil, err
		}
		config.BaseImage = baseImage

		setupCommands, err := request.RequireStringSlice("setup_commands")
		if err != nil {
			return nil, err
		}
		config.SetupCommands = setupCommands

		envs, err := request.RequireStringSlice("envs")
		if err != nil {
			return nil, err
		}
		config.Env = envs

		secrets, err := request.RequireStringSlice("secrets")
		if err != nil {
			return nil, err
		}
		config.Secrets = secrets

		if description := request.GetString("description", ""); description != "" {
			env.State.Description = description
		}

		if err := env.UpdateConfig(ctx, request.GetString("explanation", ""), config); err != nil {
			return mcp.NewToolResultErrorFromErr("unable to update the environment", err), nil
		}

		if err := repo.Update(ctx, env, "Update env "+env.ID, request.GetString("explanation", "")); err != nil {
			return mcp.NewToolResultErrorFromErr("unable to update the environment", err), nil
		}

		out, err := marshalEnvironment(env)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to marshal environment", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Environment %s updated successfully. Environment has been restarted, all previous commands have been lost.\n%s", env.ID, out)), nil
	},
}

var EnvironmentListTool = &Tool{
	Definition: mcp.NewTool("environment_list",
		mcp.WithDescription("List available environments"),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this environment is being listed."),
		),
		mcp.WithString("environment_source",
			mcp.Description("The source directory of the environment."), //  This can be a local folder (e.g. file://) or a URL to a git repository (e.g. https://github.com/user/repo.git, git@github.com:user/repo.git)"),
			mcp.Required(),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repo, err := openRepository(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the repository", err), nil
		}
		envs, err := repo.List(ctx)
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

var EnvironmentRunCmdTool = &Tool{
	Definition: mcp.NewTool("environment_run_cmd",
		mcp.WithDescription("Run a command on behalf of the user in the terminal."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this command is being run."),
		),
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
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
		repo, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
		}

		command := request.GetString("command", "")
		shell := request.GetString("shell", "sh")

		updateRepo := func() (*mcp.CallToolResult, error) {
			if err := repo.Update(ctx, env, "Run "+command, request.GetString("explanation", "")); err != nil {
				return mcp.NewToolResultErrorFromErr("failed to update repository", err), err
			}
			return nil, nil
		}

		background := request.GetBool("background", false)
		if background {
			ports := []int{}
			if portList, ok := request.GetArguments()["ports"].([]any); ok {
				for _, port := range portList {
					ports = append(ports, int(port.(float64)))
				}
			}
			endpoints, runErr := env.RunBackground(ctx, request.GetString("explanation", ""), command, shell, ports, request.GetBool("use_entrypoint", false))
			// We want to update the repository even if the command failed.
			if resp, err := updateRepo(); err != nil {
				return resp, nil
			}
			if runErr != nil {
				return mcp.NewToolResultErrorFromErr("failed to run command", err), nil
			}

			out, err := json.Marshal(endpoints)
			if err != nil {
				return nil, err
			}

			return mcp.NewToolResultText(fmt.Sprintf(`Command started in the background. Endpoints are %s

Any changes to the container workdir (%s) WILL NOT be committed to container-use/%s

Background commands are unaffected by filesystem and any other kind of changes. You need to start a new command for changes to take effect.`,
				string(out), env.Config.Workdir, env.ID)), nil
		}

		stdout, runErr := env.Run(ctx, request.GetString("explanation", ""), command, shell, request.GetBool("use_entrypoint", false))
		// We want to update the repository even if the command failed.
		if resp, err := updateRepo(); err != nil {
			return resp, nil
		}
		if runErr != nil {
			return mcp.NewToolResultErrorFromErr("failed to run command", err), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("%s\n\nAny changes to the container workdir (%s) have been committed and pushed to container-use/%s", stdout, env.Config.Workdir, env.ID)), nil
	},
}

var EnvironmentFileReadTool = &Tool{
	Definition: mcp.NewTool("environment_file_read",
		mcp.WithDescription("Read the contents of a file, specifying a line range or the entire file."),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this file is being read."),
		),
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
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
		_, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
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
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
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
		_, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
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
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
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
		repo, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
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

		if err := repo.Update(ctx, env, "Write "+targetFile, request.GetString("explanation", "")); err != nil {
			return mcp.NewToolResultErrorFromErr("unable to update the environment", err), nil
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
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
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
		repo, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
		}

		targetFile, err := request.RequireString("target_file")
		if err != nil {
			return nil, err
		}

		if err := env.FileDelete(ctx, request.GetString("explanation", ""), targetFile); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to delete file", err), nil
		}

		if err := repo.Update(ctx, env, "Delete "+targetFile, request.GetString("explanation", "")); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to update env", err), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("file %s deleted successfully, changes pushed to container-use/%s", targetFile, env.ID)), nil
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
		_, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
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

var EnvironmentAddServiceTool = &Tool{
	Definition: mcp.NewTool("environment_add_service",
		mcp.WithDescription("Add a service to the environment (e.g. database, cache, etc.)"),
		mcp.WithString("explanation",
			mcp.Description("One sentence explanation for why this service is being added."),
		),
		mcp.WithString("environment_source",
			mcp.Description("Absolute path to the source git repository for the environment."),
			mcp.Required(),
		),
		mcp.WithString("environment_id",
			mcp.Description("The ID of the environment for this command. Must call `environment_create` first."),
			mcp.Required(),
		),
		mcp.WithString("name",
			mcp.Description("The name of the service to start."),
			mcp.Required(),
		),
		mcp.WithString("image",
			mcp.Description("The image of the service to start."),
			mcp.Required(),
		),
		mcp.WithString("command",
			mcp.Description("The command to start the service. If not provided the image default command will be used."),
		),
		mcp.WithArray("ports",
			mcp.Description("Ports to expose. For each port, returns the internal (for use by other environments) and external (for use by the user) address."),
			mcp.Items(map[string]any{"type": "number"}),
		),
		mcp.WithArray("envs",
			mcp.Description("The environment variables to set (e.g. `[\"FOO=bar\", \"BAZ=qux\"]`)."),
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
			mcp.Items(map[string]any{"type": "string"}),
		),
	),
	Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repo, env, err := openEnvironment(ctx, request)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("unable to open the environment", err), nil
		}
		serviceName, err := request.RequireString("name")
		if err != nil {
			return nil, err
		}
		image, err := request.RequireString("image")
		if err != nil {
			return nil, err
		}
		command := request.GetString("command", "")
		ports := []int{}
		if portList, ok := request.GetArguments()["ports"].([]any); ok {
			for _, port := range portList {
				ports = append(ports, int(port.(float64)))
			}
		}

		envs := request.GetStringSlice("envs", []string{})
		secrets := request.GetStringSlice("secrets", []string{})

		service, err := env.AddService(ctx, request.GetString("explanation", ""), &environment.ServiceConfig{
			Name:         serviceName,
			Image:        image,
			Command:      command,
			ExposedPorts: ports,
			Env:          envs,
			Secrets:      secrets,
		})
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to add service", err), nil
		}

		if err := repo.Update(ctx, env, "Add service "+serviceName, request.GetString("explanation", "")); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to update env", err), nil
		}

		output, err := json.Marshal(service)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to marshal service", err), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("Service added and started successfully: %s", string(output))), nil
	},
}
