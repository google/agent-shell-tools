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

package main_test

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	pb "github.com/google/agent-shell-tools/grpc_exec/grpcexecpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func binPath(t *testing.T) string {
	t.Helper()
	rloc := os.Getenv("GRPC_EXECD_BIN")
	if rloc == "" {
		t.Fatal("GRPC_EXECD_BIN not set")
	}
	r, err := runfiles.New()
	if err != nil {
		t.Fatalf("runfiles: %v", err)
	}
	p, err := r.Rlocation(rloc)
	if err != nil {
		t.Fatalf("rlocation(%q): %v", rloc, err)
	}
	return p
}

// startServer launches the grpc_execd binary on a temporary Unix socket and
// returns a connected gRPC client. The server process is killed on cleanup.
func startServer(t *testing.T) pb.ExecServiceClient {
	t.Helper()
	bin := binPath(t)
	sock := filepath.Join(t.TempDir(), "exec.sock")

	cmd := exec.Command(bin, "-addr", sock)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	})

	// Wait for the socket to appear.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn, err := grpc.NewClient("unix://"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// Verify connectivity before returning.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn.Connect()
	for conn.GetState() != 2 /* READY */ {
		if !conn.WaitForStateChange(ctx, conn.GetState()) {
			t.Fatal("server did not become ready")
		}
	}

	return pb.NewExecServiceClient(conn)
}

func collect(t *testing.T, stream pb.ExecService_RunCommandClient) (output string, exitCode int32, errMsg string) {
	t.Helper()
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch e := ev.Event.(type) {
		case *pb.ServerEvent_Started:
			// Ignore.
		case *pb.ServerEvent_Output:
			output += string(e.Output)
		case *pb.ServerEvent_Exited:
			exitCode = e.Exited.GetExitCode()
			errMsg = e.Exited.GetErrorMessage()
		}
	}
}

func TestRunCommand(t *testing.T) {
	client := startServer(t)
	stream, err := client.RunCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.ClientEvent{
		Event: &pb.ClientEvent_Start{
			Start: &pb.StartCommandRequest{CommandLine: "echo hello"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	stream.CloseSend()

	output, exitCode, _ := collect(t, stream)
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
	if got := strings.TrimSpace(output); got != "hello" {
		t.Errorf("output = %q, want %q", got, "hello")
	}
}

func TestMissingAddrFlag(t *testing.T) {
	bin := binPath(t)
	cmd := exec.Command(bin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when -addr is omitted")
	}
	if !strings.Contains(string(out), "-addr is required") {
		t.Errorf("output = %q, want it to contain %q", out, "-addr is required")
	}
}

func TestSIGTERMShutdown(t *testing.T) {
	bin := binPath(t)
	sock := filepath.Join(t.TempDir(), "exec.sock")

	cmd := exec.Command(bin, "-addr", sock)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// Wait for the socket to appear.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("server exited with: %v (expected for signal exit)", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("server did not exit within 5s after SIGTERM")
	}

	// Socket should no longer accept connections.
	if _, err := net.Dial("unix", sock); err == nil {
		t.Error("socket still accepting connections after shutdown")
	}
}
