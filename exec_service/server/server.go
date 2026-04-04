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

// Package server implements the ExecService gRPC server.
package server

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	pb "github.com/google/agent-shell-tools/exec_service/execservicepb"
)

// ExecServer implements the ExecService gRPC service.
// It runs shell commands and streams their output.
type ExecServer struct {
	pb.UnimplementedExecServiceServer
}

// RunCommand executes a shell command and streams output events until the
// command exits. The command_line is interpreted by sh -c. Stdout and stderr
// are merged into the output stream.
func (s *ExecServer) RunCommand(req *pb.StartCommandRequest, stream pb.ExecService_RunCommandServer) error {
	cmd := exec.Command("sh", "-c", req.GetCommandLine())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if wd := req.GetWorkingDir(); wd != "" {
		cmd.Dir = wd
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return sendError(stream, fmt.Sprintf("pipe: %v", err))
	}
	defer pr.Close()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		return sendError(stream, fmt.Sprintf("start: %v", err))
	}
	pw.Close()

	killGroup := func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Wait for the command concurrently. When it exits, expire the pipe
	// read deadline so the read loop drains buffered output without
	// blocking on background children that inherited the pipe fds.
	waitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pr.SetReadDeadline(time.Now())
		waitCh <- err
	}()

	// Kill the process group if the client disconnects.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stream.Context().Done():
			killGroup()
		case <-done:
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&pb.ServerEvent{
				Event: &pb.ServerEvent_Output{Output: append([]byte(nil), buf[:n]...)},
			}); sendErr != nil {
				killGroup()
				<-waitCh
				return sendErr
			}
		}
		if err != nil {
			break
		}
	}

	exitCode := int32(0)
	var errMsg string
	if err := <-waitCh; err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
		} else {
			exitCode = -1
			errMsg = err.Error()
		}
	}

	return stream.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_Exited{
			Exited: &pb.ExitInfo{
				ExitCode:     exitCode,
				ErrorMessage: errMsg,
			},
		},
	})
}

func sendError(stream pb.ExecService_RunCommandServer, msg string) error {
	return stream.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_Exited{
			Exited: &pb.ExitInfo{
				ExitCode:     -1,
				ErrorMessage: msg,
			},
		},
	})
}
