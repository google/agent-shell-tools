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
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/google/agent-shell-tools/grpc_exec/grpcexecpb"
	"github.com/google/agent-shell-tools/grpc_exec/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
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
	commandID string
	output    string
	exitCode  int32
	errMsg    string
}

// startCmd opens a bidi stream and sends a StartCommandRequest, returning
// the stream for further interaction.
func startCmd(t *testing.T, client pb.ExecServiceClient, cmd, dir string) pb.ExecService_RunCommandClient {
	t.Helper()
	return startCmdCtx(t, context.Background(), client, cmd, dir)
}

func startCmdCtx(t *testing.T, ctx context.Context, client pb.ExecServiceClient, cmd, dir string) pb.ExecService_RunCommandClient {
	t.Helper()
	stream, err := client.RunCommand(ctx)
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if err := stream.Send(&pb.ClientEvent{
		Event: &pb.ClientEvent_Start{
			Start: &pb.StartCommandRequest{
				CommandLine: cmd,
				WorkingDir:  dir,
			},
		},
	}); err != nil {
		t.Fatalf("Send start: %v", err)
	}
	return stream
}

// collect reads all ServerEvents from the stream and returns the aggregated result.
func collect(t *testing.T, stream pb.ExecService_RunCommandClient) result {
	t.Helper()
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
		case *pb.ServerEvent_Started:
			r.commandID = e.Started.GetCommandId()
		case *pb.ServerEvent_Output:
			r.output += string(e.Output)
		case *pb.ServerEvent_Exited:
			r.exitCode = e.Exited.GetExitCode()
			r.errMsg = e.Exited.GetErrorMessage()
		}
	}
	return r
}

func run(t *testing.T, client pb.ExecServiceClient, cmd, dir string) result {
	t.Helper()
	stream := startCmd(t, client, cmd, dir)
	stream.CloseSend()
	return collect(t, stream)
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

func TestCommandStarted(t *testing.T) {
	c := setup(t)
	r := run(t, c, "true", "")
	if r.commandID == "" {
		t.Error("expected non-empty command_id in CommandStarted")
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
	stream := startCmd(t, c, "echo hello", "")
	stream.CloseSend()

	var gotStarted, gotOutput, gotExit bool
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Event.(type) {
		case *pb.ServerEvent_Started:
			if gotOutput || gotExit {
				t.Error("received started after output or exit event")
			}
			gotStarted = true
		case *pb.ServerEvent_Output:
			if gotExit {
				t.Error("received output after exit event")
			}
			gotOutput = true
		case *pb.ServerEvent_Exited:
			gotExit = true
		}
	}
	if !gotStarted {
		t.Error("no started event received")
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

	stream := startCmdCtx(t, ctx, c, "sleep 60", "")
	stream.CloseSend()

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

func TestFirstMessageMustBeStart(t *testing.T) {
	c := setup(t)
	stream, err := c.RunCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Send a terminate as the first message.
	if err := stream.Send(&pb.ClientEvent{
		Event: &pb.ClientEvent_Terminate{
			Terminate: &pb.TerminateCommand{Force: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for non-start first message")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.InvalidArgument {
		t.Errorf("error = %v, want InvalidArgument", err)
	}
}

func TestDuplicateStartReturnsError(t *testing.T) {
	c := setup(t)
	stream := startCmd(t, c, "sleep 60", "")

	// Wait for started.
	ev, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ev.GetStarted() == nil {
		t.Fatalf("expected started event, got %T", ev.Event)
	}

	// Send a second start — protocol violation.
	if err := stream.Send(&pb.ClientEvent{
		Event: &pb.ClientEvent_Start{
			Start: &pb.StartCommandRequest{CommandLine: "echo bad"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Drain until error.
	var gotErr error
	for {
		_, err := stream.Recv()
		if err != nil {
			gotErr = err
			break
		}
	}
	if s, ok := status.FromError(gotErr); !ok || s.Code() != codes.InvalidArgument {
		t.Errorf("error = %v, want InvalidArgument", gotErr)
	}
}

func TestTerminateForce(t *testing.T) {
	c := setup(t)
	stream := startCmd(t, c, "trap '' TERM; sleep 60", "")

	// Wait for the started event.
	ev, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ev.GetStarted() == nil {
		t.Fatalf("expected started event, got %T", ev.Event)
	}

	// Force terminate.
	if err := stream.Send(&pb.ClientEvent{
		Event: &pb.ClientEvent_Terminate{
			Terminate: &pb.TerminateCommand{Force: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Should exit promptly.
	done := make(chan result, 1)
	go func() { done <- collect(t, stream) }()

	select {
	case r := <-done:
		// SIGKILL yields exit code -1 or 137 depending on interpretation.
		if r.exitCode == 0 {
			t.Error("expected non-zero exit code after SIGKILL")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate within 5s after force terminate")
	}
}

func TestTerminateGraceful(t *testing.T) {
	c := setup(t)
	// This command exits cleanly on SIGTERM.
	stream := startCmd(t, c, "trap 'echo terminated; exit 0' TERM; echo ready; while true; do sleep 0.1; done", "")

	// Read until we see "ready".
	var buf string
	for !strings.Contains(buf, "ready") {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if out := ev.GetOutput(); len(out) > 0 {
			buf += string(out)
		}
	}

	// Send graceful terminate (SIGTERM only, no grace period).
	if err := stream.Send(&pb.ClientEvent{
		Event: &pb.ClientEvent_Terminate{
			Terminate: &pb.TerminateCommand{},
		},
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan result, 1)
	go func() { done <- collect(t, stream) }()

	select {
	case r := <-done:
		if r.exitCode != 0 {
			t.Errorf("exit code = %d, want 0", r.exitCode)
		}
		if !strings.Contains(r.output, "terminated") {
			t.Errorf("output = %q, want to contain %q", r.output, "terminated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate within 5s after graceful terminate")
	}
}

func TestTerminateGracePeriod(t *testing.T) {
	c := setup(t)
	// This command ignores SIGTERM, so the grace period SIGKILL must fire.
	stream := startCmd(t, c, "trap '' TERM; echo ready; sleep 60", "")

	// Read until we see "ready".
	var buf string
	for !strings.Contains(buf, "ready") {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if out := ev.GetOutput(); len(out) > 0 {
			buf += string(out)
		}
	}

	start := time.Now()

	// Force-kill with a 500ms grace period: SIGTERM now, SIGKILL after 500ms.
	if err := stream.Send(&pb.ClientEvent{
		Event: &pb.ClientEvent_Terminate{
			Terminate: &pb.TerminateCommand{
				Force:       true,
				GracePeriod: durationpb.New(500 * time.Millisecond),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan result, 1)
	go func() { done <- collect(t, stream) }()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		if elapsed < 400*time.Millisecond {
			t.Errorf("exited after %v, expected >= 400ms (grace period should elapse)", elapsed)
		}
		if r.exitCode == 0 {
			t.Error("expected non-zero exit code after SIGKILL")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not terminate within 10s")
	}
}

// TestInterCommandSignal starts two concurrent RPCs on the same server:
// command A waits for SIGUSR1, command B delivers it. This verifies that
// commands share a PID namespace — the property that makes the exec service
// useful inside a single sandbox.
func TestInterCommandSignal(t *testing.T) {
	c := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Command A: set the trap before printing the PID so the handler is
	// active by the time we read the PID and send the signal.
	streamA := startCmdCtx(t, ctx, c, "trap 'echo got_signal; exit 0' USR1; echo $$; while true; do sleep 0.1; done", "")
	streamA.CloseSend()

	// Read until we have the PID line.
	var buf string
	var pid string
	for pid == "" {
		ev, err := streamA.Recv()
		if err != nil {
			t.Fatalf("reading PID from command A: %v", err)
		}
		if ev.GetExited() != nil {
			t.Fatalf("command A exited before printing PID: %+v", ev.GetExited())
		}
		if out := ev.GetOutput(); len(out) > 0 {
			buf += string(out)
		}
		if i := strings.Index(buf, "\n"); i >= 0 {
			pid = strings.TrimSpace(buf[:i])
			buf = buf[i+1:]
		}
	}

	// Command B: deliver SIGUSR1 to command A.
	r := run(t, c, fmt.Sprintf("kill -USR1 %s", pid), "")
	if r.exitCode != 0 {
		t.Fatalf("kill exit code = %d, err = %q", r.exitCode, r.errMsg)
	}

	// Collect the rest of command A's output.
	var exitCode int32
	for {
		ev, err := streamA.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if out := ev.GetOutput(); len(out) > 0 {
			buf += string(out)
		}
		if info := ev.GetExited(); info != nil {
			exitCode = info.GetExitCode()
		}
	}

	if exitCode != 0 {
		t.Errorf("command A exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(buf, "got_signal") {
		t.Errorf("command A output = %q, want to contain %q", buf, "got_signal")
	}
}
