package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/shellenv"
)

// initTimeout bounds each MCP handshake RPC so a server that spawns but never
// speaks the protocol fails on a deadline instead of wedging startup (var: tests shrink it).
var initTimeout = 30 * time.Second

// callTimeout bounds each tools/call: generous (tools do real work) but a live
// server that accepts the call and never replies can't hang the loop (var: tests shrink it).
var callTimeout = 5 * time.Minute

// Client wraps an MCP client for a single server.
type Client struct {
	name      string
	client    client.MCPClient
	tools     map[string]mcp.Tool
	cancelRun context.CancelFunc // force-kills the server subprocess on close
}

// buildEnv builds environment variables from config. ${VAR} references were
// already resolved by the caller, against the in-memory secrets.
func buildEnv(envMap map[string]string) []string {
	if envMap == nil {
		return nil
	}

	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	return env
}

// NewClient creates a new MCP client for a server. WorkDir is set per-subprocess
// via WithCommandFunc (no global os.Chdir). provider may be nil: the server then
// spawns with the daemon's inherited env instead of workDir's activated toolchain.
func NewClient(ctx context.Context, name string, cfg ServerConfig, provider shellenv.Provider) (*Client, error) {
	env := buildEnv(cfg.Env)

	var opts []transport.StdioOption
	if cfg.WorkDir != "" {
		opts = append(opts, transport.WithCommandFunc(
			func(ctx context.Context, command string, envList []string, args []string) (*exec.Cmd, error) {
				if provider != nil {
					return provider.WrapExec(ctx, cfg.WorkDir, append([]string{command}, args...), envList)
				}

				cmd := exec.CommandContext(ctx, command, args...)

				cmd.Env = append(os.Environ(), envList...)
				cmd.Dir = cfg.WorkDir

				return cmd, nil
			},
		))
	}

	// runCtx owns the subprocess so Close can force-kill a mute child; detached
	// from request cancellation (a pooled server outlives one request), keeps values.
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))

	stdioTransport := transport.NewStdioWithOptions(cfg.Command, env, cfg.Args, opts...)
	if err := stdioTransport.Start(runCtx); err != nil {
		cancelRun()
		return nil, fmt.Errorf("create MCP client: %w", err)
	}

	c := client.NewClient(stdioTransport)

	log := logger.Ctx(ctx).Named("mcp.client")
	log.Debug(
		"mcp_init",
		zap.String("name", name),
		zap.String("command", cfg.Command),
		zap.String("workdir", cfg.WorkDir),
	)

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "coagent", Version: "1.0.0"}
	initReq.Params.Capabilities = mcp.ClientCapabilities{}

	toolList, err := handshake(ctx, c, initReq)
	if err != nil {
		cancelRun()

		_ = c.Close()

		return nil, err
	}

	tools := make(map[string]mcp.Tool)
	for _, tool := range toolList {
		tools[tool.Name] = tool
	}

	log.Debug("mcp_ready", zap.String("name", name), zap.Int("tools", len(tools)))

	return &Client{
		name:      name,
		client:    c,
		tools:     tools,
		cancelRun: cancelRun,
	}, nil
}

// handshake runs Initialize then ListTools, each under its own initTimeout so a
// slow-but-progressing cold start can't let one starve the other.
func handshake(ctx context.Context, c *client.Client, initReq mcp.InitializeRequest) ([]mcp.Tool, error) {
	initCtx, cancelInit := context.WithTimeout(ctx, initTimeout)
	_, err := c.Initialize(initCtx, initReq)

	cancelInit()

	if err != nil {
		return nil, fmt.Errorf("initialize MCP client: %w", err)
	}

	listCtx, cancelList := context.WithTimeout(ctx, initTimeout)
	toolsResult, err := c.ListTools(listCtx, mcp.ListToolsRequest{})

	cancelList()

	if err != nil {
		return nil, fmt.Errorf("list MCP tools: %w", err)
	}

	return toolsResult.Tools, nil
}

func (c *Client) Name() string {
	return c.name
}

func (c *Client) Tools() map[string]mcp.Tool {
	return c.tools
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	result, err := c.client.CallTool(ctx, req)
	if err != nil {
		return "", fmt.Errorf("MCP tool call: %w", err)
	}

	if result.IsError {
		if len(result.Content) > 0 {
			text := mcp.GetTextFromContent(result.Content[0])
			return "", fmt.Errorf("MCP tool error: %s", text)
		}

		return "", errors.New("MCP tool returned error")
	}

	var output string

	for _, content := range result.Content {
		if text := mcp.GetTextFromContent(content); text != "" {
			if output != "" {
				output += "\n"
			}

			output += text
		}
	}

	return output, nil
}

func (c *Client) Close() error {
	// Kill first: client.Close()'s cmd.Wait() would otherwise block forever on a
	// live server that ignores stdin-close (and callers hold pool.mu across it).
	if c.cancelRun != nil {
		c.cancelRun()
	}

	if err := c.client.Close(); err != nil {
		return fmt.Errorf("close MCP client: %w", err)
	}

	return nil
}

func (c *Client) ToolSchema(name string) (json.RawMessage, error) {
	tool, ok := c.tools[name]
	if !ok {
		return nil, fmt.Errorf("tool not found: %s", name)
	}

	schema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("marshal tool schema: %w", err)
	}

	return schema, nil
}
