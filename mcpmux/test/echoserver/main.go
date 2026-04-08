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

// echoserver is a minimal MCP server for testing. It exposes an "echo" tool
// that returns its input as-is, and a "slow_echo" tool that sleeps before
// responding.
package main

import (
	"context"
	"log"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type EchoInput struct {
	Message string `json:"message"`
}

type SlowEchoInput struct {
	Message  string `json:"message"`
	DelayMs  int    `json:"delay_ms"`
}

func handleEcho(_ context.Context, _ *mcp.CallToolRequest, input EchoInput) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: input.Message}},
	}, nil, nil
}

func handleSlowEcho(ctx context.Context, _ *mcp.CallToolRequest, input SlowEchoInput) (*mcp.CallToolResult, any, error) {
	select {
	case <-time.After(time.Duration(input.DelayMs) * time.Millisecond):
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: input.Message}},
	}, nil, nil
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "echoserver", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Returns input as-is"}, handleEcho)
	mcp.AddTool(server, &mcp.Tool{Name: "slow_echo", Description: "Returns input after a delay"}, handleSlowEcho)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
