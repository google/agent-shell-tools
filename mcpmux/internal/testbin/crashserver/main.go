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

// crashserver is an MCP server that exposes a "crash" tool which calls
// os.Exit(1). Used for crash detection testing.
package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type CrashInput struct{}

func handleCrash(_ context.Context, _ *mcp.CallToolRequest, _ CrashInput) (*mcp.CallToolResult, any, error) {
	os.Exit(1)
	return nil, nil, nil // unreachable
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "crashserver", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "crash", Description: "Crashes the server"}, handleCrash)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
