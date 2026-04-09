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
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func echoServerPath(t *testing.T) string {
	t.Helper()
	p, err := runfiles.Rlocation("_main/mcpmux/internal/testbin/echoserver/echoserver_/echoserver")
	if err != nil {
		t.Fatalf("finding echoserver runfile: %v", err)
	}
	return p
}

func crashServerPath(t *testing.T) string {
	t.Helper()
	p, err := runfiles.Rlocation("_main/mcpmux/internal/testbin/crashserver/crashserver_/crashserver")
	if err != nil {
		t.Fatalf("finding crashserver runfile: %v", err)
	}
	return p
}

func TestChildStart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	child := NewChild(ChildConfig{
		Name:    "test-echo",
		Command: echoServerPath(t),
	}, logger, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := child.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer child.Stop()

	if child.Status() != StatusRunning {
		t.Errorf("status = %v, want running", child.Status())
	}
	if child.PID() == 0 {
		t.Error("PID should be nonzero")
	}

	// echoserver exposes "echo" and "slow_echo".
	wantTools := map[string]bool{"echo": false, "slow_echo": false}
	for _, tool := range tools {
		if _, ok := wantTools[tool.Name]; ok {
			wantTools[tool.Name] = true
		}
	}
	for name, found := range wantTools {
		if !found {
			t.Errorf("tool %q not discovered", name)
		}
	}
}

func TestChildCallTool(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	child := NewChild(ChildConfig{
		Name:    "test-echo",
		Command: echoServerPath(t),
	}, logger, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := child.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer child.Stop()

	result, err := child.Session().CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hello"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatal("CallTool returned error result")
	}
	if len(result.Content) == 0 {
		t.Fatal("no content in result")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", result.Content[0])
	}
	if tc.Text != "hello" {
		t.Errorf("echo result = %q, want %q", tc.Text, "hello")
	}
}

func TestChildStop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	child := NewChild(ChildConfig{
		Name:    "test-echo",
		Command: echoServerPath(t),
	}, logger, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := child.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := child.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if child.Session() != nil {
		t.Error("session should be nil after stop")
	}
}

func TestChildCrashDetection(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var crashed atomic.Bool
	child := NewChild(ChildConfig{
		Name:    "test-crash",
		Command: crashServerPath(t),
	}, logger, func() { crashed.Store(true) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := child.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Call the crash tool — this makes the server os.Exit(1).
	_, _ = child.Session().CallTool(ctx, &mcp.CallToolParams{
		Name:      "crash",
		Arguments: map[string]any{},
	})

	// Wait for crash detection.
	deadline := time.After(5 * time.Second)
	for !crashed.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for crash detection")
		case <-time.After(50 * time.Millisecond):
		}
	}

	if child.Status() != StatusCrashed {
		t.Errorf("status = %v, want crashed", child.Status())
	}
}
