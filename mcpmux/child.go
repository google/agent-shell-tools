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
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ChildConfig holds the spawn configuration for a child MCP server.
type ChildConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
}

// ChildStatus represents the lifecycle state of a child server.
type ChildStatus int

const (
	StatusNew      ChildStatus = iota // created, not yet started
	StatusStarting                    // Start in progress
	StatusRunning                     // healthy and connected
	StatusStopped                     // intentionally shut down
	StatusCrashed                     // exited unexpectedly
)

func (s ChildStatus) String() string {
	switch s {
	case StatusNew:
		return "new"
	case StatusStarting:
		return "starting"
	case StatusRunning:
		return "running"
	case StatusStopped:
		return "stopped"
	case StatusCrashed:
		return "crashed"
	default:
		return "unknown"
	}
}

// Child manages a single child MCP server process.
type Child struct {
	Config    ChildConfig
	StartTime time.Time

	mu         sync.RWMutex
	status     ChildStatus
	tools      []*mcp.Tool // discovered tools (original names)
	pid        int
	process    *os.Process
	session    *mcp.ClientSession
	stopCancel context.CancelFunc // cancels an in-progress Start
	startDone  chan struct{}      // closed when Start completes
	logger     *slog.Logger
	onCrash    func() // called when the child process crashes
}

// NewChild creates a new Child from the given config.
func NewChild(cfg ChildConfig, logger *slog.Logger, onCrash func()) *Child {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Child{
		Config:  cfg,
		status:  StatusNew,
		logger:  logger.With("child", cfg.Name),
		onCrash: onCrash,
	}
}

// Start spawns the child process, performs the MCP handshake, and discovers
// tools. It starts a background goroutine to monitor the child for crashes.
// Each Child instance may only be started once (StatusNew → StatusStarting).
func (c *Child) Start(ctx context.Context) ([]*mcp.Tool, error) {
	c.mu.Lock()
	if c.status != StatusNew {
		c.mu.Unlock()
		return nil, fmt.Errorf("child %q: cannot start from status %v", c.Config.Name, c.status)
	}
	c.status = StatusStarting
	// Derive a context that Stop can cancel to abort startup.
	ctx, cancel := context.WithCancel(ctx)
	c.stopCancel = cancel
	c.startDone = make(chan struct{})
	c.mu.Unlock()

	tools, err := c.start(ctx)

	c.mu.Lock()
	if err != nil {
		// Only transition to Crashed for real failures, not cancellation.
		// Stop sets Stopped; caller cancellation leaves us in Starting
		// (which is terminal for a single-use Child).
		if c.status == StatusStarting && ctx.Err() == nil {
			c.status = StatusCrashed
		}
	}
	c.stopCancel = nil
	close(c.startDone)
	c.mu.Unlock()
	cancel()
	if err != nil {
		return nil, err
	}
	return tools, nil
}

func (c *Child) start(ctx context.Context) ([]*mcp.Tool, error) {
	cmd := exec.Command(c.Config.Command, c.Config.Args...)
	if c.Config.Cwd != "" {
		cmd.Dir = c.Config.Cwd
	}
	cmd.Env = os.Environ()
	for k, v := range c.Config.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = &prefixWriter{prefix: []byte("[" + c.Config.Name + "] "), w: os.Stderr}

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(
		&mcp.Implementation{Name: "mcpmux", Version: "v1"},
		nil,
	)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		// CommandTransport may have started the process before the MCP
		// handshake failed. Kill it to avoid leaking an orphan.
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		return nil, fmt.Errorf("connect to child %q: %w", c.Config.Name, err)
	}

	// Publish session and process immediately so Stop() can reach them
	// if it races with tool discovery.
	c.mu.Lock()
	if c.status != StatusStarting {
		c.mu.Unlock()
		session.Close()
		return nil, fmt.Errorf("child %q: stopped during startup", c.Config.Name)
	}
	c.session = session
	c.process = cmd.Process
	c.pid = cmd.Process.Pid
	c.mu.Unlock()

	tools, err := c.discoverTools(ctx, session)
	if err != nil {
		// Stop() may already be cleaning up; only close if we still own the session.
		c.mu.Lock()
		if c.session == session {
			c.session = nil
			c.process = nil
		}
		c.mu.Unlock()
		session.Close()
		return nil, fmt.Errorf("discover tools from child %q: %w", c.Config.Name, err)
	}

	c.mu.Lock()
	if c.status != StatusStarting {
		c.mu.Unlock()
		return nil, fmt.Errorf("child %q: stopped during startup", c.Config.Name)
	}
	c.tools = tools
	c.status = StatusRunning
	c.StartTime = time.Now()
	c.mu.Unlock()

	c.logger.Info("child started", "pid", cmd.Process.Pid, "tools", len(tools))

	go c.monitor()

	return tools, nil
}

func (c *Child) discoverTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var all []*mcp.Tool
	var cursor string
	for {
		result, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return all, nil
}

// monitor waits for the child session to close unexpectedly.
func (c *Child) monitor() {
	c.mu.RLock()
	session := c.session
	c.mu.RUnlock()
	if session == nil {
		return
	}

	_ = session.Wait()

	c.mu.Lock()
	wasCrash := c.status == StatusRunning
	if wasCrash {
		c.status = StatusCrashed
		c.logger.Warn("child crashed")
	}
	c.mu.Unlock()

	if wasCrash && c.onCrash != nil {
		c.onCrash()
	}
}

// stopTimeout is how long Stop waits for a graceful session.Close before
// killing the process. Exported for testing.
var stopTimeout = 5 * time.Second

// Stop shuts down the child process. It first attempts a graceful close
// (which does stdin close → SIGTERM → SIGKILL via the SDK). If the
// graceful close blocks (e.g. due to in-flight tool calls), it kills
// the process after a timeout to unblock it.
func (c *Child) Stop() error {
	c.mu.Lock()
	session := c.session
	process := c.process
	stopCancel := c.stopCancel
	startDone := c.startDone
	c.session = nil
	c.process = nil
	c.stopCancel = nil
	// Preserve StatusCrashed if the child already died unexpectedly.
	if c.status != StatusCrashed {
		c.status = StatusStopped
	}
	c.mu.Unlock()

	// If Start is still in progress, cancel it and wait for cleanup.
	if stopCancel != nil {
		stopCancel()
	}
	if startDone != nil {
		<-startDone
	}
	if session == nil {
		// Re-read: Start may have published session before our cancel took effect.
		c.mu.Lock()
		session = c.session
		process = c.process
		c.session = nil
		c.process = nil
		c.mu.Unlock()
	}
	if session == nil {
		return nil
	}
	c.logger.Info("stopping child")

	// Try graceful close. If it blocks (in-flight requests), kill after timeout.
	done := make(chan error, 1)
	go func() { done <- session.Close() }()

	select {
	case err := <-done:
		return err
	case <-time.After(stopTimeout):
		c.logger.Warn("graceful close timed out, killing child")
		if process != nil {
			process.Kill()
		}
		// Wait for session.Close to finish now that the process is dead.
		<-done
		return nil
	}
}

// Status returns the current status.
func (c *Child) Status() ChildStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// Tools returns the discovered tools.
func (c *Child) Tools() []*mcp.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tools
}

// PID returns the child's process ID.
func (c *Child) PID() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pid
}

// Session returns the MCP client session for making tool calls.
func (c *Child) Session() *mcp.ClientSession {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.session
}

// prefixWriter wraps a writer and prefixes each write with a string.
// It does not attempt line-buffering; each Write call gets one prefix.
type prefixWriter struct {
	prefix []byte
	w      io.Writer
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	if _, err := pw.w.Write(pw.prefix); err != nil {
		return 0, err
	}
	return pw.w.Write(p)
}
