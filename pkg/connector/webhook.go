package connector

import (
	"context"
	"fmt"
	"net/http"

	"maunium.net/go/mautrix/bridgev2"
)

// webhookPath is the route Talk posts bot events to. It is registered under
// the bridge's own HTTP server, so the full URL given to `occ talk:bot:install`
// is <public address>/_nctalk/webhook.
const webhookPath = "/_nctalk"

// registerWebhook mounts the Talk bot webhook receiver on the bridge's HTTP
// server.
func (nc *NCTalkConnector) registerWebhook(_ context.Context) error {
	server, ok := nc.Bridge.Matrix.(bridgev2.MatrixConnectorWithServer)
	if !ok || server.GetRouter() == nil {
		return fmt.Errorf("the Matrix connector does not expose an HTTP server, which is required to receive Talk bot webhooks")
	}

	router := http.NewServeMux()
	router.HandleFunc("POST /webhook", nc.handleWebhook)
	server.GetRouter().Handle(webhookPath+"/", http.StripPrefix(webhookPath, router))
	return nil
}

// handleWebhook receives a Talk bot event.
//
// Talk gives the bot five seconds to respond and counts any status other than
// 200 or 202 as an error, disabling the bot after repeated failures. So this
// handler must verify and enqueue only, never do Matrix or OCS work inline.
func (nc *NCTalkConnector) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Implemented in M1.
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
