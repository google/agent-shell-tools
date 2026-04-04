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

package server_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/google/agent-shell-tools/exec_service/execservicepb"
	"github.com/google/agent-shell-tools/exec_service/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func setup(t *testing.T) pb.ExecServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	pb.RegisterExecServiceServer(s, &server.ExecServer{})
	go s.Serve(lis)
	t.Cleanup(s.GracefulStop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewExecServiceClient(conn)
}

type result struct {
	output   string
	exitCode int32
	errMsg   string
}

func run(t *testing.T, client pb.ExecServiceClient, cmd, dir string) result {
	t.Helper()
	return runCtx(t, context.Background(), client, cmd, dir)
}

func runCtx(t *testing.T, ctx context.Context, client pb.ExecServiceClient, cmd, dir string) result {
	t.Helper()
	stream, err := client.RunCommand(ctx, &pb.StartCommandRequest{
		CommandLine: cmd,
		WorkingDir:  dir,
	})
	if err != nil {
		t.Fatalf("RunCommand(%q): %v", cmd, err)
	}
	var r result
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch e := ev.Event.(type) {
		case *pb.ServerEvent_Output:
			r.output += string(e.Output)
		case *pb.ServerEvent_Exited:
			r.exitCode = e.Exited.GetExitCode()
			r.errMsg = e.Exited.GetErrorMessage()
		}
	}
	return r
}

func TestEcho(t *testing.T) {
	c := setup(t)
	r := run(t, c, "echo hello", "")
	if r.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", r.exitCode)
	}
	if got := strings.TrimSpace(r.output); got != "hello" {
		t.Errorf("output = %q, want %q", got, "hello")
	}
}

func TestExitCode(t *testing.T) {
	c := setup(t)
	r := run(t, c, "exit 42", "")
	if r.exitCode != 42 {
		t.Errorf("exit code = %d, want 42", r.exitCode)
	}
}

func TestExitCodeZero(t *testing.T) {
	c := setup(t)
	r := run(t, c, "true", "")
	if r.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", r.exitCode)
	}
}

func TestWorkingDir(t *testing.T) {
	c := setup(t)
	r := run(t, c, "pwd", "/tmp")
	if got := strings.TrimSpace(r.output); got != "/tmp" {
		t.Errorf("pwd = %q, want /tmp", got)
	}
}

func TestBadWorkingDir(t *testing.T) {
	c := setup(t)
	r := run(t, c, "echo hi", "/nonexistent_dir_12345")
	if r.errMsg == "" {
		t.Error("expected error message for bad working dir")
	}
	if r.exitCode >= 0 {
		t.Errorf("exit code = %d, want negative", r.exitCode)
	}
}

func TestCommandNotFound(t *testing.T) {
	c := setup(t)
	r := run(t, c, "nonexistent_command_12345", "")
	if r.exitCode == 0 {
		t.Error("expected non-zero exit code for missing command")
	}
}

func TestStderrInOutput(t *testing.T) {
	c := setup(t)
	r := run(t, c, "echo err >&2", "")
	if got := strings.TrimSpace(r.output); got != "err" {
		t.Errorf("output = %q, want %q", got, "err")
	}
}

func TestMixedOutput(t *testing.T) {
	c := setup(t)
	// Use a subshell to ensure ordering.
	r := run(t, c, "echo out && echo err >&2", "")
	if !strings.Contains(r.output, "out") || !strings.Contains(r.output, "err") {
		t.Errorf("output = %q, want both 'out' and 'err'", r.output)
	}
}

func TestLargeOutput(t *testing.T) {
	c := setup(t)
	// Generate ~100KB of output to test streaming.
	r := run(t, c, "dd if=/dev/zero bs=1024 count=100 2>/dev/null | base64", "")
	if r.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", r.exitCode)
	}
	if len(r.output) < 100*1024 {
		t.Errorf("output length = %d, want >= %d", len(r.output), 100*1024)
	}
}

func TestEmptyCommand(t *testing.T) {
	c := setup(t)
	r := run(t, c, "", "")
	if r.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", r.exitCode)
	}
}

func TestMultilineOutput(t *testing.T) {
	c := setup(t)
	r := run(t, c, "printf 'a\\nb\\nc\\n'", "")
	if r.output != "a\nb\nc\n" {
		t.Errorf("output = %q, want %q", r.output, "a\nb\nc\n")
	}
}

func TestStreamingOrder(t *testing.T) {
	c := setup(t)
	stream, err := c.RunCommand(context.Background(), &pb.StartCommandRequest{
		CommandLine: "echo hello",
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotOutput, gotExit bool
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Event.(type) {
		case *pb.ServerEvent_Output:
			if gotExit {
				t.Error("received output after exit event")
			}
			gotOutput = true
		case *pb.ServerEvent_Exited:
			gotExit = true
		}
	}
	if !gotOutput {
		t.Error("no output events received")
	}
	if !gotExit {
		t.Error("no exit event received")
	}
}

func TestBackgroundChild(t *testing.T) {
	c := setup(t)
	start := time.Now()
	r := run(t, c, "sleep 60 & echo done", "")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("took %v, expected < 5s (background child should not block)", elapsed)
	}
	if r.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", r.exitCode)
	}
	if got := strings.TrimSpace(r.output); got != "done" {
		t.Errorf("output = %q, want %q", got, "done")
	}
}

func TestCancellation(t *testing.T) {
	c := setup(t)
	ctx, cancel := context.WithCancel(context.Background())

	stream, err := c.RunCommand(ctx, &pb.StartCommandRequest{
		CommandLine: "sleep 60",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Give the process time to start, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// The stream should terminate promptly after cancellation.
	done := make(chan struct{})
	go func() {
		for {
			_, err := stream.Recv()
			if err != nil {
				break
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate within 5s after cancellation")
	}
}
