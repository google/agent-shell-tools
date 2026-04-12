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

package main

import (
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/google/agent-shell-tools/grpc_exec/grpcexecpb"
	"github.com/google/agent-shell-tools/grpc_exec/server"
	"google.golang.org/grpc"
)

// startTestServer starts an ExecService gRPC server on a temporary Unix socket
// and returns the socket path. The server is stopped on test cleanup.
func startTestServer(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "test.sock")

	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer()
	pb.RegisterExecServiceServer(srv, &server.ExecServer{})
	go srv.Serve(lis)
	t.Cleanup(srv.GracefulStop)

	return sock
}

func TestBasicCommand(t *testing.T) {
	sock := startTestServer(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-addr", sock, "echo", "hello"}, &stdout, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "hello" {
		t.Errorf("stdout = %q, want %q", got, "hello")
	}
}

func TestExitCode(t *testing.T) {
	sock := startTestServer(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-addr", sock, "exit 42"}, &stdout, &stderr)

	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
}

func TestWorkingDir(t *testing.T) {
	sock := startTestServer(t)
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{"-addr", sock, "-dir", dir, "pwd"}, &stdout, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != dir {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

func TestMissingAddr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"echo", "hello"}, &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-addr is required") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "-addr is required")
	}
}

func TestMissingCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-addr", "/tmp/fake.sock"}, &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "command is required") {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), "command is required")
	}
}

func TestStderrOutput(t *testing.T) {
	sock := startTestServer(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-addr", sock, "echo error >&2"}, &stdout, &stderr)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// Server merges stderr into stdout stream.
	if got := strings.TrimSpace(stdout.String()); got != "error" {
		t.Errorf("stdout = %q, want %q", got, "error")
	}
}

func TestArgQuoting(t *testing.T) {
	sock := startTestServer(t)
	dir := t.TempDir()

	// "touch" with a filename containing a space should create one file, not two.
	var stdout, stderr bytes.Buffer
	code := run([]string{"-addr", sock, "-dir", dir, "touch", "a b"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify exactly one file named "a b" was created.
	var check bytes.Buffer
	code = run([]string{"-addr", sock, "-dir", dir, "ls"}, &check, &stderr)
	if code != 0 {
		t.Fatalf("ls exit code = %d; stderr: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(check.String()); got != "a b" {
		t.Errorf("ls = %q, want %q", got, "a b")
	}
}

func TestCommandNotFound(t *testing.T) {
	sock := startTestServer(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-addr", sock, "nonexistent_command_xyz"}, &stdout, &stderr)

	if code == 0 {
		t.Error("exit code = 0, want non-zero for command not found")
	}
}
