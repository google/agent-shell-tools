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

// Package mcpmux implements a meta-MCP proxy that manages multiple child MCP
// servers behind a single unified MCP interface.
package mcpmux

import (
	"errors"
	"strings"
)

const namespaceSep = "__"

// ValidateServerName checks that a server name is valid for use as a namespace
// prefix. It must be non-empty and must not contain the namespace separator.
func ValidateServerName(name string) error {
	if name == "" {
		return errors.New("server name must not be empty")
	}
	if strings.Contains(name, namespaceSep) {
		return errors.New("server name must not contain \"__\"")
	}
	if name[len(name)-1] == '_' {
		return errors.New("server name must not end with \"_\"")
	}
	return nil
}

// PrefixTool returns the namespaced tool name "{server}__{tool}".
func PrefixTool(server, tool string) string {
	return server + namespaceSep + tool
}

// SplitTool splits a namespaced tool name on the first "__" separator.
// Returns the server name, the original tool name, and true if the name
// contained the separator. Returns ("", name, false) otherwise.
func SplitTool(name string) (server, tool string, ok bool) {
	i := strings.Index(name, namespaceSep)
	if i < 0 {
		return "", name, false
	}
	return name[:i], name[i+len(namespaceSep):], true
}
