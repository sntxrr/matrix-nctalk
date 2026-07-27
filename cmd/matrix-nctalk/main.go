// matrix-nctalk - A Matrix–Nextcloud Talk puppeting bridge.
// Copyright (C) 2026 Don O'Neill
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// Command matrix-nctalk is a Matrix ↔ Nextcloud Talk bridge.
package main

import (
	"os"

	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/sntxrr/matrix-nctalk/pkg/connector"
)

// Build-time metadata, injected with -ldflags.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

var version = "0.1.1"

func main() {
	// Handled before the bridge's own flag parsing, which does not know about
	// subcommands and would reject this one as an unknown argument.
	if len(os.Args) > 1 && os.Args[1] == "bot-install" {
		os.Exit(runBotInstall(os.Args[2:], os.Stdout, os.Stderr))
	}

	m := mxmain.BridgeMain{
		Name:        "matrix-nctalk",
		Description: "A Matrix bridge for Nextcloud Talk",
		URL:         "https://github.com/sntxrr/matrix-nctalk",
		Version:     version,
		Connector:   &connector.NCTalkConnector{},
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
