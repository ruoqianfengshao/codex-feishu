package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFromEnvReadsCodexChatsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Codex")
	t.Setenv("CTR_GO_CONFIG", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("CTR_GO_CODEX_CHATS_ROOT", root)

	cfg := FromEnv()

	if cfg.CodexChatsRoot != root {
		t.Fatalf("CodexChatsRoot = %q, want %q", cfg.CodexChatsRoot, root)
	}
}

func TestMarshalJSONIncludesDesktopOpenSettings(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(Config{NotifyNewRun: true, NotifySystem: true, OpenCodexDesktopOnFeishu: true})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if got["notify_new_run"] != true {
		t.Fatalf("notify_new_run = %#v, want true", got["notify_new_run"])
	}
	if got["notify_system"] != true {
		t.Fatalf("notify_system = %#v, want true", got["notify_system"])
	}
	if got["open_codex_desktop_on_feishu"] != true {
		t.Fatalf("open_codex_desktop_on_feishu = %#v, want true", got["open_codex_desktop_on_feishu"])
	}
}

func TestFromEnvReadsFeishuConfig(t *testing.T) {
	t.Setenv("CTR_GO_CONFIG", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("CTR_GO_ADAPTER", "feishu")
	t.Setenv("CTR_GO_FEISHU_APP_ID", "cli_test")
	t.Setenv("CTR_GO_FEISHU_APP_SECRET", "secret")
	t.Setenv("CTR_GO_FEISHU_ALLOWED_OPEN_IDS", "ou_1,ou_2 ou_1")
	t.Setenv("CTR_GO_FEISHU_ALLOWED_CHAT_IDS", "oc_1;oc_2")
	t.Setenv("CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU", "true")

	cfg := FromEnv()

	if cfg.Adapter != "feishu" {
		t.Fatalf("Adapter = %q, want feishu", cfg.Adapter)
	}
	if cfg.FeishuAppID != "cli_test" || cfg.FeishuAppSecret != "secret" {
		t.Fatalf("Feishu credentials = %q/%q", cfg.FeishuAppID, cfg.FeishuAppSecret)
	}
	if !reflect.DeepEqual(cfg.FeishuAllowedOpenIDs, []string{"ou_1", "ou_2"}) {
		t.Fatalf("FeishuAllowedOpenIDs = %#v", cfg.FeishuAllowedOpenIDs)
	}
	if !reflect.DeepEqual(cfg.FeishuAllowedChatIDs, []string{"oc_1", "oc_2"}) {
		t.Fatalf("FeishuAllowedChatIDs = %#v", cfg.FeishuAllowedChatIDs)
	}
	if !cfg.OpenCodexDesktopOnFeishu {
		t.Fatal("OpenCodexDesktopOnFeishu = false, want true")
	}
}

func TestMarshalJSONRedactsFeishuSecret(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(Config{Adapter: "feishu", FeishuAppID: "cli_test", FeishuAppSecret: "secret"})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "secret") {
		t.Fatalf("MarshalJSON leaked Feishu secret: %s", text)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if got["feishu_configured"] != true {
		t.Fatalf("feishu_configured = %#v, want true", got["feishu_configured"])
	}
}

func TestParseEnvFileSupportsCommentsAndQuotes(t *testing.T) {
	t.Parallel()

	values, err := ParseEnvFile([]byte(`
# comment
CTR_GO_FEISHU_APP_ID="cli token with spaces"
CTR_GO_FEISHU_ALLOWED_OPEN_IDS='ou_123,ou_456'
CTR_GO_NOTIFY_NEW_RUN=off
`), "test.env")
	if err != nil {
		t.Fatalf("ParseEnvFile failed: %v", err)
	}
	want := map[string]string{
		"CTR_GO_FEISHU_APP_ID":           "cli token with spaces",
		"CTR_GO_FEISHU_ALLOWED_OPEN_IDS": "ou_123,ou_456",
		"CTR_GO_NOTIFY_NEW_RUN":          "off",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %#v, want %#v", values, want)
	}
}

func TestParseEnvFileRejectsInvalidLine(t *testing.T) {
	t.Parallel()

	_, err := ParseEnvFile([]byte("not-an-assignment\n"), "bad.env")
	if err == nil {
		t.Fatal("ParseEnvFile succeeded, want invalid line error")
	}
	if !strings.Contains(err.Error(), "expected KEY=VALUE") {
		t.Fatalf("error = %v, want KEY=VALUE message", err)
	}
}

func TestLoadReadsConfigFileAndEnvOverridesIt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	fileDefaultCWD := filepath.Join(dir, "from-file")
	envDefaultCWD := filepath.Join(dir, "from-env")
	home := filepath.Join(dir, "home")
	if err := os.WriteFile(configPath, []byte(strings.Join([]string{
		`CTR_GO_HOME="` + home + `"`,
		`CTR_GO_DEFAULT_CWD="` + fileDefaultCWD + `"`,
		`CTR_GO_NOTIFY_NEW_RUN="off"`,
		`CTR_GO_NOTIFY_SYSTEM="off"`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("CTR_GO_DEFAULT_CWD", envDefaultCWD)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.DefaultCWD != envDefaultCWD {
		t.Fatalf("DefaultCWD = %q, want env override %q", cfg.DefaultCWD, envDefaultCWD)
	}
	if cfg.Paths.Home != home {
		t.Fatalf("Home = %q, want %q", cfg.Paths.Home, home)
	}
	if cfg.NotifyNewRun {
		t.Fatal("NotifyNewRun = true, want false from config file")
	}
	if cfg.NotifySystem {
		t.Fatal("NotifySystem = true, want false from config file")
	}
}

func TestLoadAppliesRuntimeProxyEnvFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	proxy := "http://127.0.0.1:18080"
	if err := os.WriteFile(configPath, []byte(strings.Join([]string{
		`CTR_GO_HOME="` + filepath.Join(dir, "home") + `"`,
		`HTTPS_PROXY="` + proxy + `"`,
		`NODE_USE_ENV_PROXY="1"`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NODE_USE_ENV_PROXY", "")

	if _, err := Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := os.Getenv("HTTPS_PROXY"); got != proxy {
		t.Fatalf("HTTPS_PROXY = %q, want %q", got, proxy)
	}
	if got := os.Getenv("NODE_USE_ENV_PROXY"); got != "1" {
		t.Fatalf("NODE_USE_ENV_PROXY = %q, want 1", got)
	}
}

func TestLoadDoesNotOverrideExplicitRuntimeProxyEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	if err := os.WriteFile(configPath, []byte(`HTTPS_PROXY="http://file-proxy"`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("HTTPS_PROXY", "http://env-proxy")

	if _, err := Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := os.Getenv("HTTPS_PROXY"); got != "http://env-proxy" {
		t.Fatalf("HTTPS_PROXY = %q, want env value", got)
	}
}

func TestConfigFilePathOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.env")
	t.Setenv("CTR_GO_CONFIG", path)

	if got := ConfigFilePath(); got != path {
		t.Fatalf("ConfigFilePath = %q, want %q", got, path)
	}
}
