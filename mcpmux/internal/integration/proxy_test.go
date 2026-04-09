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

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func binPath(t *testing.T, name string) string {
	t.Helper()
	p, err := runfiles.Rlocation(name)
	if err != nil {
		t.Fatalf("rlocation %s: %v", name, err)
	}
	return p
}

func proxyPath(t *testing.T) string {
	return binPath(t, "_main/mcpmux/mcpmux_/mcpmux")
}

func echoServerPath(t *testing.T) string {
	return binPath(t, "_main/mcpmux/internal/testbin/echoserver/echoserver_/echoserver")
}

func echoServerV2Path(t *testing.T) string {
	return binPath(t, "_main/mcpmux/internal/testbin/echoserver_v2/echoserver_v2_/echoserver_v2")
}

func crashServerPath(t *testing.T) string {
	return binPath(t, "_main/mcpmux/internal/testbin/crashserver/crashserver_/crashserver")
}

// startProxy starts the mcpmux binary as a subprocess and connects an MCP client.
func startProxy(t *testing.T) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(proxyPath(t))
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to proxy: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
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

func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatal("no content")
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T", r.Content[0])
	}
	return tc.Text
}

func TestEndToEndAddAndCall(t *testing.T) {
	session := startProxy(t)

	// Add echo server.
	r := callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})
	if r.IsError {
		t.Fatalf("add_server: %s", resultText(t, r))
	}

	// Verify tools appear via list.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	toolsResult, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := false
	for _, tool := range toolsResult.Tools {
		if tool.Name == "echo__echo" {
			found = true
			break
		}
	}
	if !found {
		t.Error("echo__echo not found in tools list")
	}

	// Call the proxied tool.
	r = callTool(t, session, "echo__echo", map[string]any{"message": "integration"})
	if r.IsError {
		t.Fatalf("echo__echo: %s", resultText(t, r))
	}
	if got := resultText(t, r); got != "integration" {
		t.Errorf("echo = %q, want %q", got, "integration")
	}

	// Remove server.
	r = callTool(t, session, "remove_server", map[string]any{"name": "echo"})
	if r.IsError {
		t.Fatalf("remove_server: %s", resultText(t, r))
	}

	// Call removed tool → error.
	_, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo__echo",
		Arguments: map[string]any{"message": "gone"},
	})
	if err == nil {
		t.Error("calling removed tool should return error")
	}
}

func TestEndToEndReload(t *testing.T) {
	session := startProxy(t)

	// Add v1 echo server.
	r := callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})
	if r.IsError {
		t.Fatalf("add_server: %s", resultText(t, r))
	}

	// Verify no reverse tool.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo__reverse",
		Arguments: map[string]any{"message": "hello"},
	})
	if err == nil {
		t.Fatal("reverse should not exist yet")
	}

	// Swap to v2 binary, then reload. Since reload_server preserves the
	// original config, we first remove+add with the v2 binary to test the
	// full reload_server path on v2.
	callTool(t, session, "remove_server", map[string]any{"name": "echo"})
	r = callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerV2Path(t),
	})
	if r.IsError {
		t.Fatalf("add_server v2: %s", resultText(t, r))
	}

	// Now exercise reload_server end-to-end.
	r = callTool(t, session, "reload_server", map[string]any{"name": "echo"})
	if r.IsError {
		t.Fatalf("reload_server: %s", resultText(t, r))
	}

	// Verify reverse tool works.
	r = callTool(t, session, "echo__reverse", map[string]any{"message": "hello"})
	if r.IsError {
		t.Fatalf("echo__reverse: %s", resultText(t, r))
	}
	if got := resultText(t, r); got != "olleh" {
		t.Errorf("reverse = %q, want %q", got, "olleh")
	}
}

func TestEndToEndCrashRecovery(t *testing.T) {
	session := startProxy(t)

	// Add crash server.
	r := callTool(t, session, "add_server", map[string]any{
		"name":    "crashy",
		"command": crashServerPath(t),
	})
	if r.IsError {
		t.Fatalf("add_server: %s", resultText(t, r))
	}

	// Trigger crash.
	callTool(t, session, "crashy__crash", map[string]any{})

	// Poll until crash is detected.
	var listResult struct {
		Servers []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"servers"`
	}
	deadline := time.After(5 * time.Second)
	for {
		r = callTool(t, session, "list_servers", map[string]any{})
		text := resultText(t, r)
		if err := json.Unmarshal([]byte(text), &listResult); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(listResult.Servers) == 1 && listResult.Servers[0].Status == "crashed" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for crash, got: %s", text)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Reload should bring it back.
	r = callTool(t, session, "reload_server", map[string]any{"name": "crashy"})
	if r.IsError {
		t.Fatalf("reload_server: %s", resultText(t, r))
	}

	// Verify it's running again.
	r = callTool(t, session, "list_servers", map[string]any{})
	afterText := resultText(t, r)
	if err := json.Unmarshal([]byte(afterText), &listResult); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(listResult.Servers) != 1 || listResult.Servers[0].Status != "running" {
		t.Fatalf("expected running server, got: %s", afterText)
	}
}

func TestConcurrentCalls(t *testing.T) {
	session := startProxy(t)

	r := callTool(t, session, "add_server", map[string]any{
		"name":    "echo",
		"command": echoServerPath(t),
	})
	if r.IsError {
		t.Fatalf("add_server: %s", resultText(t, r))
	}

	// Fire 10 concurrent calls.
	var wg sync.WaitGroup
	errs := make(chan string, 10)
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			msg := fmt.Sprintf("hello-%d", i)
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "echo__echo",
				Arguments: map[string]any{"message": msg},
			})
			if err != nil {
				errs <- fmt.Sprintf("goroutine %d: %v", i, err)
				return
			}
			if result.IsError {
				errs <- fmt.Sprintf("goroutine %d: tool error: %s", i, result.Content)
				return
			}
			if len(result.Content) == 0 {
				errs <- fmt.Sprintf("goroutine %d: no content", i)
				return
			}
			tc, ok := result.Content[0].(*mcp.TextContent)
			if !ok || tc.Text != msg {
				errs <- fmt.Sprintf("goroutine %d: got %v, want %q", i, result.Content[0], msg)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
