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

package mcpmux

import "testing"

func TestValidateServerName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"myserver", false},
		{"my-server", false},
		{"server123", false},
		{"日本語", false},
		{"", true},
		{"has__double", true},
		{"__leading", true},
		{"trailing__", true},
		{"trailing_", true},
	}
	for _, tt := range tests {
		err := ValidateServerName(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateServerName(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestPrefixTool(t *testing.T) {
	tests := []struct {
		server, tool, want string
	}{
		{"myserver", "echo", "myserver__echo"},
		{"srv", "tool__sub", "srv__tool__sub"},
		{"日本語", "ツール", "日本語__ツール"},
	}
	for _, tt := range tests {
		got := PrefixTool(tt.server, tt.tool)
		if got != tt.want {
			t.Errorf("PrefixTool(%q, %q) = %q, want %q", tt.server, tt.tool, got, tt.want)
		}
	}
}

func TestSplitTool(t *testing.T) {
	tests := []struct {
		name       string
		wantServer string
		wantTool   string
		wantOK     bool
	}{
		{"myserver__echo", "myserver", "echo", true},
		{"srv__tool__sub", "srv", "tool__sub", true},
		{"日本語__ツール", "日本語", "ツール", true},
		{"notool", "", "notool", false},
		{"__leadingsep", "", "leadingsep", true},
	}
	for _, tt := range tests {
		server, tool, ok := SplitTool(tt.name)
		if server != tt.wantServer || tool != tt.wantTool || ok != tt.wantOK {
			t.Errorf("SplitTool(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.name, server, tool, ok, tt.wantServer, tt.wantTool, tt.wantOK)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	server, tool := "myserver", "echo"
	prefixed := PrefixTool(server, tool)
	gotServer, gotTool, ok := SplitTool(prefixed)
	if !ok || gotServer != server || gotTool != tool {
		t.Errorf("round-trip failed: PrefixTool(%q, %q) = %q, SplitTool → (%q, %q, %v)",
			server, tool, prefixed, gotServer, gotTool, ok)
	}
}
