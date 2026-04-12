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

// Binary grpc_exec is a command line client for the ExecService gRPC server.
// It connects to the server over a Unix socket, sends a command, streams
// output to stdout, and exits with the command's exit code.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	pb "github.com/google/agent-shell-tools/grpc_exec/grpcexecpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("grpc_exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "Unix socket path to connect to (required)")
	dir := fs.String("dir", "", "Working directory for the command")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *addr == "" {
		fmt.Fprintln(stderr, "-addr is required")
		return 2
	}

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintln(stderr, "command is required")
		return 2
	}

	// When the caller passes a single argument it is treated as a raw shell
	// command (e.g. grpc_exec -addr s "echo hello && ls"). Multiple
	// arguments are shell-quoted so that spaces and metacharacters in
	// individual args are preserved (e.g. grpc_exec -addr s touch "a b").
	var cmdLine string
	if len(cmdArgs) == 1 {
		cmdLine = cmdArgs[0]
	} else {
		cmdLine = shellJoin(cmdArgs)
	}

	sockPath, err := filepath.Abs(*addr)
	if err != nil {
		fmt.Fprintf(stderr, "resolve socket path: %v\n", err)
		return 1
	}

	conn, err := grpc.NewClient("unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fmt.Fprintf(stderr, "dial: %v\n", err)
		return 1
	}
	defer conn.Close()

	client := pb.NewExecServiceClient(conn)
	stream, err := client.RunCommand(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "RunCommand: %v\n", err)
		return 1
	}

	if err := stream.Send(&pb.ClientEvent{
		Event: &pb.ClientEvent_Start{
			Start: &pb.StartCommandRequest{
				CommandLine: cmdLine,
				WorkingDir:  *dir,
			},
		},
	}); err != nil {
		fmt.Fprintf(stderr, "send start: %v\n", err)
		return 1
	}
	stream.CloseSend()

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(stderr, "recv: %v\n", err)
			return 1
		}
		switch e := ev.Event.(type) {
		case *pb.ServerEvent_Started:
			// Ignore for CLI usage.
		case *pb.ServerEvent_Output:
			stdout.Write(e.Output)
		case *pb.ServerEvent_Exited:
			if msg := e.Exited.GetErrorMessage(); msg != "" {
				fmt.Fprintf(stderr, "error: %s\n", msg)
			}
			return int(e.Exited.GetExitCode())
		}
	}
	return 0
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// shellJoin quotes each argument and joins them with spaces, producing a
// string safe for interpretation by sh -c.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
