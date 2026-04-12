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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	pb "github.com/google/agent-shell-tools/grpc_exec/grpcexecpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExecServer implements the ExecService gRPC service.
// It runs shell commands and streams their output.
type ExecServer struct {
	pb.UnimplementedExecServiceServer
}

// RunCommand executes a shell command and streams output events until the
// command exits. The first ClientEvent must be a StartCommandRequest.
// Subsequent TerminateCommand events signal the running process.
func (s *ExecServer) RunCommand(stream pb.ExecService_RunCommandServer) error {
	// The first message must be a start request.
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return status.Error(codes.InvalidArgument, "empty stream: first message must be StartCommandRequest")
		}
		return err
	}
	req := first.GetStart()
	if req == nil {
		return status.Error(codes.InvalidArgument, "first message must be StartCommandRequest")
	}

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

	pid := cmd.Process.Pid
	killPG := func() { syscall.Kill(-pid, syscall.SIGKILL) }
	termPG := func() { syscall.Kill(-pid, syscall.SIGTERM) }

	// Send CommandStarted with a random ID.
	cmdID := randomID()
	if err := stream.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_Started{
			Started: &pb.CommandStarted{CommandId: cmdID},
		},
	}); err != nil {
		killPG()
		cmd.Wait()
		return err
	}

	// Wait for the command concurrently. When it exits, kill the
	// process group so that background children close their inherited
	// pipe fds, allowing the read loop to reach EOF cleanly. This is
	// pipe cleanup, not termination policy — it runs regardless of how
	// the process exited. The deadline is a fallback for children that
	// escaped the group (e.g. via setsid).
	waitCh := make(chan error, 1)
	procDone := make(chan struct{})
	go func() {
		err := cmd.Wait()
		close(procDone)
		killPG()
		pr.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		waitCh <- err
	}()

	// Listen for client events (terminate) and context cancellation (disconnect).
	protoErr := make(chan error, 1)
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				// io.EOF means the client closed its send side — normal,
				// the command keeps running. Any other error means the
				// stream is broken.
				if err != io.EOF {
					select {
					case <-procDone:
					default:
						killPG()
					}
				}
				return
			}
			switch ev.Event.(type) {
			case *pb.ClientEvent_Terminate:
				t := ev.GetTerminate()
				select {
				case <-procDone:
					continue
				default:
				}
				if !t.GetForce() {
					termPG()
				} else if gp := t.GetGracePeriod(); gp != nil && gp.AsDuration() > 0 {
					termPG()
					go func() {
						select {
						case <-time.After(gp.AsDuration()):
							killPG()
						case <-procDone:
						}
					}()
				} else {
					killPG()
				}
			default:
				// Protocol violation: only terminate is valid after start.
				killPG()
				protoErr <- status.Error(codes.InvalidArgument, "only TerminateCommand is valid after StartCommandRequest")
				return
			}
		}
	}()

	// Kill the process group if the client disconnects.
	go func() {
		select {
		case <-stream.Context().Done():
			killPG()
		case <-procDone:
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&pb.ServerEvent{
				Event: &pb.ServerEvent_Output{Output: append([]byte(nil), buf[:n]...)},
			}); sendErr != nil {
				killPG()
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

	// If the recv goroutine detected a protocol violation, return it
	// instead of the normal exit event.
	select {
	case err := <-protoErr:
		return err
	default:
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

func randomID() string {
	b := make([]byte, 16)
	io.ReadFull(rand.Reader, b)
	return fmt.Sprintf("%x", b)
}
