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
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectProxy creates a Proxy and connects an in-memory MCP client to it.
func connectProxy(t *testing.T) (*Proxy, *mcp.ClientSession) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	proxy := NewProxy(logger)

	t1, t2 := mcp.NewInMemoryTransports()
	go proxy.Server().Run(context.Background(), t1)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	session, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatalf("connect to proxy: %v", err)
	}
	t.Cleanup(func() {
		session.Close()
		proxy.Shutdown()
	})
	return proxy, session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", name, err)
	}
	return result
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("no content in result")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", result.Content[0])
	}
	return tc.Text
}

func TestProxyAddAndList(t *testing.T) {
	_, session := connectProxy(t)

	// Add echoserver.
	result := callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})
	if result.IsError {
		t.Fatalf("add_server failed: %s", resultText(t, result))
	}

	// List servers.
	result = callTool(t, session, "list_servers", map[string]any{})
	text := resultText(t, result)
	var listResult struct {
		Servers []serverInfo `json:"servers"`
	}
	if err := json.Unmarshal([]byte(text), &listResult); err != nil {
		t.Fatalf("unmarshal list_servers: %v", err)
	}
	if len(listResult.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(listResult.Servers))
	}
	srv := listResult.Servers[0]
	if srv.Name != "echo" {
		t.Errorf("server name = %q, want %q", srv.Name, "echo")
	}
	if srv.Status != "running" {
		t.Errorf("server status = %q, want %q", srv.Status, "running")
	}
}

func TestProxyToolProxying(t *testing.T) {
	_, session := connectProxy(t)

	callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})

	// Call the proxied tool.
	result := callTool(t, session, "echo__echo", map[string]any{
		"message": "hello proxy",
	})
	if result.IsError {
		t.Fatalf("echo__echo failed: %s", resultText(t, result))
	}
	got := resultText(t, result)
	if got != "hello proxy" {
		t.Errorf("echo result = %q, want %q", got, "hello proxy")
	}
}

func TestProxyRemoveServer(t *testing.T) {
	_, session := connectProxy(t)

	callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})

	result := callTool(t, session, "remove_server", map[string]any{
		"name": "echo",
	})
	if result.IsError {
		t.Fatalf("remove_server failed: %s", resultText(t, result))
	}

	// Calling the removed tool should fail with a protocol error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo__echo",
		Arguments: map[string]any{"message": "hello"},
	})
	if err == nil {
		t.Fatal("calling removed tool should return an error")
	}
}

func TestProxyDuplicateAdd(t *testing.T) {
	_, session := connectProxy(t)

	callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})

	result := callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})
	if !result.IsError {
		t.Fatal("duplicate add_server should return error")
	}
}

func TestProxyRemoveNonexistent(t *testing.T) {
	_, session := connectProxy(t)

	result := callTool(t, session, "remove_server", map[string]any{
		"name": "noexist",
	})
	if !result.IsError {
		t.Fatal("remove nonexistent should return error")
	}
}

func TestProxyReload(t *testing.T) {
	_, session := connectProxy(t)

	callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})

	result := callTool(t, session, "reload_server", map[string]any{
		"name": "echo",
	})
	if result.IsError {
		t.Fatalf("reload_server failed: %s", resultText(t, result))
	}

	// Verify tools still work after reload.
	result = callTool(t, session, "echo__echo", map[string]any{
		"message": "after reload",
	})
	if result.IsError {
		t.Fatalf("echo after reload failed: %s", resultText(t, result))
	}
	got := resultText(t, result)
	if got != "after reload" {
		t.Errorf("echo result = %q, want %q", got, "after reload")
	}
}

func TestProxyTwoServersNamespacing(t *testing.T) {
	_, session := connectProxy(t)

	callTool(t, session, "add_server", map[string]any{
		"name":    "a",
		"command": echoServerPath(t),
	})
	callTool(t, session, "add_server", map[string]any{
		"name":    "b",
		"command": echoServerPath(t),
	})

	// Both should work independently.
	r1 := callTool(t, session, "a__echo", map[string]any{"message": "from a"})
	r2 := callTool(t, session, "b__echo", map[string]any{"message": "from b"})

	if resultText(t, r1) != "from a" {
		t.Errorf("a__echo = %q, want %q", resultText(t, r1), "from a")
	}
	if resultText(t, r2) != "from b" {
		t.Errorf("b__echo = %q, want %q", resultText(t, r2), "from b")
	}

	// Remove one; other should still work.
	callTool(t, session, "remove_server", map[string]any{"name": "a"})
	r2 = callTool(t, session, "b__echo", map[string]any{"message": "still works"})
	if resultText(t, r2) != "still works" {
		t.Errorf("b__echo after removing a = %q", resultText(t, r2))
	}
}
