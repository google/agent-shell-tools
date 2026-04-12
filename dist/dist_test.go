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

package dist_test

import (
	"archive/tar"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// extractTar unpacks the dist tarball into dir.
func extractTar(t *testing.T, dir string) {
	t.Helper()
	rloc := os.Getenv("DIST_TAR")
	if rloc == "" {
		t.Fatal("DIST_TAR not set")
	}
	r, err := runfiles.New()
	if err != nil {
		t.Fatalf("runfiles: %v", err)
	}
	p, err := r.Rlocation(rloc)
	if err != nil {
		t.Fatalf("rlocation(%q): %v", rloc, err)
	}

	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open tarball: %v", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		dest := filepath.Join(dir, hdr.Name)
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, hdr.FileInfo().Mode())
		if err != nil {
			t.Fatalf("create %s: %v", hdr.Name, err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			t.Fatalf("write %s: %v", hdr.Name, err)
		}
		out.Close()
	}
}

// TestSandboxedHostname extracts the dist tarball, runs grpc_execd inside the
// sandbox, and uses grpc_exec to verify that the UTS namespace hostname is
// "coding-agent".
func TestSandboxedHostname(t *testing.T) {
	dir := t.TempDir()
	extractTar(t, dir)

	bin := func(name string) string { return filepath.Join(dir, name) }
	sock := filepath.Join(dir, "exec.sock")

	// Start grpc_execd inside the sandbox.
	server := exec.Command(bin("sandbox"), "--log-file", "/dev/null", "--rw", dir, "--",
		bin("grpc_execd"), "-addr", sock)
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		t.Fatalf("start sandboxed server: %v", err)
	}
	t.Cleanup(func() {
		server.Process.Kill()
		server.Wait()
	})

	// Wait for the socket to appear.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Use grpc_exec to read the hostname inside the sandbox.
	out, err := exec.Command(bin("grpc_exec"), "-addr", sock, "cat /proc/sys/kernel/hostname").Output()
	if err != nil {
		t.Fatalf("exec_client: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "coding-agent" {
		t.Errorf("hostname = %q, want %q", got, "coding-agent")
	}
}
