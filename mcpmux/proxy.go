// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mcpmux

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Proxy is the core MCP proxy that manages child servers and exposes their
// tools through a single unified MCP server.
type Proxy struct {
	server *mcp.Server
	logger *slog.Logger

	mu       sync.RWMutex
	children map[string]*Child // keyed by server name
}

// NewProxy creates a new Proxy with management tools registered.
func NewProxy(logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	server := mcp.NewServer(
		&mcp.Implementation{Name: "mcpmux", Version: "v1"},
		&mcp.ServerOptions{},
	)
	p := &Proxy{
		server:   server,
		logger:   logger,
		children: make(map[string]*Child),
	}
	p.registerManagementTools()
	return p
}

// Server returns the underlying MCP server (for running with a transport).
func (p *Proxy) Server() *mcp.Server { return p.server }

// Shutdown stops all child servers.
func (p *Proxy) Shutdown() {
	p.mu.Lock()
	children := make(map[string]*Child, len(p.children))
	for k, v := range p.children {
		children[k] = v
	}
	p.children = make(map[string]*Child)
	p.mu.Unlock()

	for name, child := range children {
		p.logger.Info("shutting down child", "name", name)
		child.Stop()
	}
}

// -- Management tool input types --

type addServerInput struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
}

type nameInput struct {
	Name string `json:"name"`
}

// -- Management tool registration --

func (p *Proxy) registerManagementTools() {
	mcp.AddTool(p.server, &mcp.Tool{
		Name:        "add_server",
		Description: "Add and start a child MCP server",
	}, p.handleAddServer)

	mcp.AddTool(p.server, &mcp.Tool{
		Name:        "remove_server",
		Description: "Stop and remove a child MCP server",
	}, p.handleRemoveServer)

	mcp.AddTool(p.server, &mcp.Tool{
		Name:        "reload_server",
		Description: "Restart a child MCP server with the same config",
	}, p.handleReloadServer)

	mcp.AddTool(p.server, &mcp.Tool{
		Name:        "list_servers",
		Description: "List all registered child MCP servers",
	}, p.handleListServers)
}

// -- Management tool handlers --

func (p *Proxy) handleAddServer(ctx context.Context, req *mcp.CallToolRequest, input addServerInput) (*mcp.CallToolResult, any, error) {
	if err := ValidateServerName(input.Name); err != nil {
		return textResult(true, err.Error()), nil, nil
	}

	p.mu.Lock()
	if _, exists := p.children[input.Name]; exists {
		p.mu.Unlock()
		return textResult(true, fmt.Sprintf("server %q already exists", input.Name)), nil, nil
	}

	cfg := ChildConfig{
		Name:    input.Name,
		Command: input.Command,
		Args:    input.Args,
		Env:     input.Env,
		Cwd:     input.Cwd,
	}
	var child *Child
	child = NewChild(cfg, p.logger, func() { p.handleCrash(input.Name, child) })
	p.children[input.Name] = child
	p.mu.Unlock()

	tools, err := child.Start(ctx)
	if err != nil {
		p.mu.Lock()
		delete(p.children, input.Name)
		p.mu.Unlock()
		return textResult(true, fmt.Sprintf("failed to start server %q: %v", input.Name, err)), nil, nil
	}

	// Check that the child hasn't crashed between Start returning and now.
	if child.Status() != StatusRunning {
		p.mu.Lock()
		delete(p.children, input.Name)
		p.mu.Unlock()
		return textResult(true, fmt.Sprintf("server %q exited immediately after starting", input.Name)), nil, nil
	}

	// Register proxied tools on the MCP server.
	var toolNames []string
	for _, tool := range tools {
		nsName := PrefixTool(input.Name, tool.Name)
		toolNames = append(toolNames, nsName)
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		p.server.AddTool(
			&mcp.Tool{
				Name:        nsName,
				Description: tool.Description,
				InputSchema: schema,
			},
			p.makeProxyHandler(input.Name, tool.Name),
		)
	}

	result, _ := json.Marshal(map[string]any{
		"server": input.Name,
		"tools":  toolNames,
	})
	return textResult(false, string(result)), nil, nil
}

func (p *Proxy) handleRemoveServer(_ context.Context, _ *mcp.CallToolRequest, input nameInput) (*mcp.CallToolResult, any, error) {
	p.mu.Lock()
	child, exists := p.children[input.Name]
	if !exists {
		p.mu.Unlock()
		return textResult(true, fmt.Sprintf("server %q not found", input.Name)), nil, nil
	}
	delete(p.children, input.Name)
	p.mu.Unlock()

	// Remove namespaced tools.
	p.removeToolsForChild(child, input.Name)

	child.Stop()
	return textResult(false, fmt.Sprintf("removed server %q", input.Name)), nil, nil
}

func (p *Proxy) handleReloadServer(ctx context.Context, req *mcp.CallToolRequest, input nameInput) (*mcp.CallToolResult, any, error) {
	p.mu.RLock()
	old, exists := p.children[input.Name]
	if !exists {
		p.mu.RUnlock()
		return textResult(true, fmt.Sprintf("server %q not found", input.Name)), nil, nil
	}
	cfg := old.Config
	p.mu.RUnlock()

	// Remove old server.
	removeResult, _, _ := p.handleRemoveServer(ctx, req, input)
	if removeResult.IsError {
		return removeResult, nil, nil
	}

	// Add with the original config.
	return p.handleAddServer(ctx, req, addServerInput{
		Name:    cfg.Name,
		Command: cfg.Command,
		Args:    cfg.Args,
		Env:     cfg.Env,
		Cwd:     cfg.Cwd,
	})
}

type serverInfo struct {
	Name          string   `json:"name"`
	Command       string   `json:"command"`
	Args          []string `json:"args,omitempty"`
	Status        string   `json:"status"`
	Tools         []string `json:"tools"`
	PID           int      `json:"pid"`
	UptimeSeconds float64  `json:"uptime_seconds"`
}

func (p *Proxy) handleListServers(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var servers []serverInfo
	for name, child := range p.children {
		var toolNames []string
		if child.Status() == StatusRunning {
			for _, t := range child.Tools() {
				toolNames = append(toolNames, PrefixTool(name, t.Name))
			}
		}
		var uptime float64
		if !child.StartTime.IsZero() {
			uptime = time.Since(child.StartTime).Seconds()
		}
		servers = append(servers, serverInfo{
			Name:          name,
			Command:       child.Config.Command,
			Args:          child.Config.Args,
			Status:        child.Status().String(),
			Tools:         toolNames,
			PID:           child.PID(),
			UptimeSeconds: uptime,
		})
	}

	result, _ := json.Marshal(map[string]any{"servers": servers})
	return textResult(false, string(result)), nil, nil
}

// -- Tool proxying --

func (p *Proxy) makeProxyHandler(serverName, toolName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		p.mu.RLock()
		child, exists := p.children[serverName]
		p.mu.RUnlock()

		if !exists {
			return textResult(true, fmt.Sprintf("server %q not found", serverName)), nil
		}

		session := child.Session()
		if session == nil {
			return textResult(true, fmt.Sprintf("server %q is not running", serverName)), nil
		}

		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      toolName,
			Arguments: req.Params.Arguments,
		})
		if err != nil {
			return textResult(true, fmt.Sprintf("call to %s/%s failed: %v", serverName, toolName, err)), nil
		}
		return result, nil
	}
}

// -- Crash handling --

func (p *Proxy) handleCrash(name string, crashed *Child) {
	p.logger.Warn("child server crashed, removing tools", "name", name)

	p.mu.RLock()
	current := p.children[name]
	p.mu.RUnlock()

	// Only remove tools if the crashed child is still the current one
	// (not replaced by a reload).
	if current == crashed {
		p.removeToolsForChild(crashed, name)
	}
}

func (p *Proxy) removeToolsForChild(child *Child, name string) {
	var names []string
	for _, t := range child.Tools() {
		names = append(names, PrefixTool(name, t.Name))
	}
	if len(names) > 0 {
		p.server.RemoveTools(names...)
	}
}

// -- Helpers --

func textResult(isError bool, text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: isError,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}
