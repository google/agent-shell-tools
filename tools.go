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

//go:build tools

// Package tools pins external Go modules that are built only by Bazel
// (siso, cipd) so that `go mod tidy` keeps them and their transitive
// requires in go.mod for gazelle's go_deps.from_file extension.
package tools

import (
	// Mirror siso's main package imports so `go mod tidy` keeps every
	// transitive dep needed to build @org_chromium_go_build_siso//:siso.
	// (siso's `main` package can't be imported directly.)
	_ "go.chromium.org/build/siso/hashfs/osfs"
	_ "go.chromium.org/build/siso/subcmd/auth"
	_ "go.chromium.org/build/siso/subcmd/collector"
	_ "go.chromium.org/build/siso/subcmd/fetch"
	_ "go.chromium.org/build/siso/subcmd/fscmd"
	_ "go.chromium.org/build/siso/subcmd/isolate"
	_ "go.chromium.org/build/siso/subcmd/metricscmd"
	_ "go.chromium.org/build/siso/subcmd/ninja"
	_ "go.chromium.org/build/siso/subcmd/ninjafrontend"
	_ "go.chromium.org/build/siso/subcmd/proxy"
	_ "go.chromium.org/build/siso/subcmd/ps"
	_ "go.chromium.org/build/siso/subcmd/query"
	_ "go.chromium.org/build/siso/subcmd/recall"
	_ "go.chromium.org/build/siso/subcmd/report"
	_ "go.chromium.org/build/siso/subcmd/sandbox"
	_ "go.chromium.org/build/siso/subcmd/scandeps"
	_ "go.chromium.org/build/siso/subcmd/webui"

	// Same for cipd. cli/main.go imports every subcommand including proxy.
	_ "go.chromium.org/luci/cipd/client/cli"
)
