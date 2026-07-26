// Command matrix-nctalk is a Matrix ↔ Nextcloud Talk bridge.
package main

import (
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/sntxrr/matrix-nextcloud/pkg/connector"
)

// Build-time metadata, injected with -ldflags.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

var version = "0.1.0"

func main() {
	m := mxmain.BridgeMain{
		Name:        "matrix-nctalk",
		Description: "A Matrix bridge for Nextcloud Talk",
		URL:         "https://github.com/sntxrr/matrix-nextcloud",
		Version:     version,
		Connector:   &connector.NCTalkConnector{},
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
