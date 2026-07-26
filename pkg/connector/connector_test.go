package connector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// GetName is called before the config is loaded, so it must not depend on it.
func TestGetNameWorksWithoutConfig(t *testing.T) {
	nc := &NCTalkConnector{}
	name := nc.GetName()

	if name.NetworkID == "" || name.DisplayName == "" || name.BeeperBridgeType == "" {
		t.Errorf("incomplete bridge name: %+v", name)
	}
	if !strings.HasPrefix(name.DefaultCommandPrefix, "!") {
		t.Errorf("command prefix %q must start with !", name.DefaultCommandPrefix)
	}
	if name.DefaultPort == 0 {
		t.Error("a default port is needed for the example config")
	}
	// An invalid mxc:// URI is worse than none, so it should be left empty
	// until a real icon is uploaded.
	if name.NetworkIcon != "" && !strings.HasPrefix(string(name.NetworkIcon), "mxc://") {
		t.Errorf("network icon %q is not an mxc URI", name.NetworkIcon)
	}
}

func TestGetCapabilities(t *testing.T) {
	nc := &NCTalkConnector{}
	caps := nc.GetCapabilities()
	if caps == nil {
		t.Fatal("expected capabilities")
	}
	// Talk marks a conversation read as a whole and does not implicitly mark
	// sent messages as read, so the bridge has to do it.
	if !caps.ImplicitReadReceipts {
		t.Error("ImplicitReadReceipts should be set for Talk")
	}
}

func TestGetBridgeInfoVersion(t *testing.T) {
	info, capabilities := (&NCTalkConnector{}).GetBridgeInfoVersion()
	if info < 1 || capabilities < 1 {
		t.Errorf("versions must be positive, got %d and %d", info, capabilities)
	}
}

func TestGetDBMetaTypes(t *testing.T) {
	types := (&NCTalkConnector{}).GetDBMetaTypes()

	if _, ok := types.UserLogin().(*UserLoginMetadata); !ok {
		t.Error("UserLogin metadata type is wrong")
	}
	if _, ok := types.Portal().(*PortalMetadata); !ok {
		t.Error("Portal metadata type is wrong")
	}
	if _, ok := types.Ghost().(*GhostMetadata); !ok {
		t.Error("Ghost metadata type is wrong")
	}
	if _, ok := types.Message().(*MessageMetadata); !ok {
		t.Error("Message metadata type is wrong")
	}
}

func TestInitSetsUpHTTPClient(t *testing.T) {
	nc := &NCTalkConnector{}
	bridge := &bridgev2.Bridge{Log: zerolog.Nop()}
	nc.Init(bridge)

	if nc.Bridge != bridge {
		t.Error("Init did not store the bridge")
	}
	if nc.HTTP == nil {
		t.Fatal("Init did not create an HTTP client")
	}
	// An unbounded timeout would let a stalled Nextcloud pin a worker forever.
	if nc.HTTP.Timeout == 0 {
		t.Error("the HTTP client should have a timeout")
	}
}

// Starting without a bot secret cannot work, and failing loudly at startup is
// far better than silently rejecting every webhook later.
func TestStartRequiresBotConfiguration(t *testing.T) {
	nc := &NCTalkConnector{Bridge: &bridgev2.Bridge{Log: zerolog.Nop()}}

	err := nc.Start(context.Background())
	if err == nil {
		t.Fatal("expected an error with no bot secret configured")
	}
	if !strings.Contains(err.Error(), "bot_secret") {
		t.Errorf("the error should name the missing setting, got %v", err)
	}

	nc.Config.BotSecret = "secret"
	err = nc.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bot_name") {
		t.Errorf("expected a complaint about bot_name, got %v", err)
	}
}

func TestBotClientFor(t *testing.T) {
	nc := &NCTalkConnector{Config: Config{BotSecret: "secret"}}
	nc.Init(&bridgev2.Bridge{Log: zerolog.Nop()})

	bot := nc.botClientFor("https://cloud.example.com/")
	if bot.BaseURL != "https://cloud.example.com" {
		t.Errorf("BaseURL = %q", bot.BaseURL)
	}
	if bot.Secret != "secret" {
		t.Error("the configured bot secret was not used")
	}
}

// The example config must actually parse into the config struct, or the bridge
// would ship a file that cannot be loaded.
func TestExampleConfigParses(t *testing.T) {
	example, data, upgrader := (&NCTalkConnector{}).GetConfig()
	if example == "" {
		t.Fatal("no example config")
	}
	if data == nil || upgrader == nil {
		t.Fatal("GetConfig must return a target and an upgrader")
	}

	var cfg Config
	if err := yaml.Unmarshal([]byte(example), &cfg); err != nil {
		t.Fatalf("the example config does not parse: %v", err)
	}

	if cfg.BotName == "" {
		t.Error("the example should ship a default bot name")
	}
	if !cfg.AutoEnableBot {
		t.Error("auto_enable_bot should default to on")
	}
	if cfg.RelayUnlinkedUsers {
		t.Error("relay_unlinked_users should default to off so nothing is misattributed")
	}
	if cfg.LoginTimeout <= 0 {
		t.Error("the example should ship a positive login timeout")
	}
	if cfg.BotSecret != "" {
		t.Error("the example must not ship a bot secret")
	}
}

// The documented occ command and the webhook route have to agree, or the bot
// will post to a path the bridge does not serve.
func TestExampleConfigDocumentsTheWebhookPath(t *testing.T) {
	example, _, _ := (&NCTalkConnector{}).GetConfig()
	if !strings.Contains(example, webhookPath+"/webhook") {
		t.Errorf("the example config should document the %s/webhook route", webhookPath)
	}
	if !strings.Contains(example, "talk:bot:install") {
		t.Error("the example config should show the occ install command")
	}
}

// The upgrader carries user values forward when the config file is rewritten;
// if it misses a field, that setting is silently reset on every upgrade.
func TestUpgradeConfigPreservesAllFields(t *testing.T) {
	example, _, upgrader := (&NCTalkConnector{}).GetConfig()

	var base yaml.Node
	if err := yaml.Unmarshal([]byte(example), &base); err != nil {
		t.Fatalf("example config does not parse: %v", err)
	}

	existing := []byte(`
bot_secret: my-secret
bot_name: My Bridge
auto_enable_bot: false
allowed_servers:
  - cloud.example.com
relay_unlinked_users: true
login_timeout: 5m
`)
	var cfg yaml.Node
	if err := yaml.Unmarshal(existing, &cfg); err != nil {
		t.Fatalf("existing config does not parse: %v", err)
	}

	upgrader.DoUpgrade(configupgrade.NewHelper(&base, &cfg))

	var got Config
	out, err := yaml.Marshal(&base)
	if err != nil {
		t.Fatalf("marshal upgraded config: %v", err)
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("upgraded config does not parse: %v", err)
	}

	if got.BotSecret != "my-secret" {
		t.Errorf("bot_secret = %q, was not preserved", got.BotSecret)
	}
	if got.BotName != "My Bridge" {
		t.Errorf("bot_name = %q, was not preserved", got.BotName)
	}
	if got.AutoEnableBot {
		t.Error("auto_enable_bot = true, the user's false was not preserved")
	}
	if !got.RelayUnlinkedUsers {
		t.Error("relay_unlinked_users = false, the user's true was not preserved")
	}
	if len(got.AllowedServers) != 1 || got.AllowedServers[0] != "cloud.example.com" {
		t.Errorf("allowed_servers = %v, was not preserved", got.AllowedServers)
	}
	if got.LoginTimeout != 5*time.Minute {
		t.Errorf("login_timeout = %v, was not preserved", got.LoginTimeout)
	}
}

func TestLoadUserLogin(t *testing.T) {
	nc := &NCTalkConnector{Config: Config{BotSecret: "secret"}}
	nc.Init(newQuietBridge())

	login := &bridgev2.UserLogin{
		UserLogin: &database.UserLogin{
			ID: makeUserLoginID("cloud.example.com", "alice"),
			Metadata: &UserLoginMetadata{
				ServerURL:   "https://cloud.example.com",
				Username:    "alice",
				AppPassword: "pw",
			},
		},
		Log: zerolog.Nop(),
	}

	if err := nc.LoadUserLogin(context.Background(), login); err != nil {
		t.Fatalf("LoadUserLogin failed: %v", err)
	}
	client, ok := login.Client.(*NCTalkClient)
	if !ok {
		t.Fatalf("client = %T, want *NCTalkClient", login.Client)
	}
	if client.Client.BaseURL != "https://cloud.example.com" || client.Client.Username != "alice" {
		t.Errorf("OCS client not configured: %+v", client.Client)
	}
	if client.Bot == nil || client.Bot.Secret != "secret" {
		t.Error("bot client not configured with the shared secret")
	}
}

func TestLoadUserLoginRejectsIncompleteMetadata(t *testing.T) {
	nc := &NCTalkConnector{}
	nc.Init(newQuietBridge())

	incomplete := &bridgev2.UserLogin{
		UserLogin: &database.UserLogin{
			ID:       makeUserLoginID("cloud.example.com", "alice"),
			Metadata: &UserLoginMetadata{Username: "alice"}, // no server URL
		},
		Log: zerolog.Nop(),
	}
	if err := nc.LoadUserLogin(context.Background(), incomplete); err == nil {
		t.Error("expected an error for metadata with no server URL")
	}

	wrongType := &bridgev2.UserLogin{
		UserLogin: &database.UserLogin{
			ID:       makeUserLoginID("cloud.example.com", "alice"),
			Metadata: &PortalMetadata{},
		},
		Log: zerolog.Nop(),
	}
	if err := nc.LoadUserLogin(context.Background(), wrongType); err == nil {
		t.Error("expected an error for the wrong metadata type")
	}
}
