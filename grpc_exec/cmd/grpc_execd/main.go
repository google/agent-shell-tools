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

// Binary grpc_execd runs the ExecService gRPC server.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/google/agent-shell-tools/grpc_exec/grpcexecpb"
	"github.com/google/agent-shell-tools/grpc_exec/server"
	"google.golang.org/grpc"
)

func main() {
	addr := flag.String("addr", "", "Unix socket path to listen on (required)")
	flag.Parse()

	if *addr == "" {
		log.Fatal("-addr is required")
	}

	lis, err := net.Listen("unix", *addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", *addr, err)
	}

	srv := grpc.NewServer()
	pb.RegisterExecServiceServer(srv, &server.ExecServer{})

	// GracefulStop waits for active RPCs to finish. If a RunCommand stream
	// is executing a long-lived child process, the server will block until
	// that command exits (or an external SIGKILL arrives).
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		srv.GracefulStop()
	}()

	log.Printf("ExecService listening on unix %s", *addr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("Server exited with error: %v", err)
	}
}
