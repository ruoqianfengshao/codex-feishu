package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	qrterminal "github.com/mdp/qrterminal/v3"
	"rsc.io/qr"

	"github.com/ruoqianfengshao/codex-feishu/internal/config"
	"github.com/ruoqianfengshao/codex-feishu/internal/daemon"
	"github.com/ruoqianfengshao/codex-feishu/internal/feishu"
	"github.com/ruoqianfengshao/codex-feishu/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.SetOutput(os.Stderr)
		log.Printf("ctr-go: %s", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return runWithIO(args, os.Stdin, os.Stdout)
}

func runWithIO(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		printUsage(out)
		return nil
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], in, out)
	case "feishu":
		return runFeishu(args[1:], in, out)
	case "service":
		return runService(args[1:], in, out)
	case "daemon":
		if len(args) < 2 || args[1] != "run" {
			return errors.New("usage: ctr-go daemon run")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return runDaemon(cfg)
	case "status":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return runStatus(cfg, out)
	case "doctor":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return runDoctor(cfg, out)
	case "repair":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return runRepair(cfg, out)
	case "version":
		_, _ = fmt.Fprintf(out, "ctr-go v%s\n", version.Version)
		return nil
	case "help", "--help", "-h":
		printUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown command: %s", strings.Join(args, " "))
	}
}

func runFeishu(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		printFeishuUsage(out)
		return nil
	}
	switch args[0] {
	case "setup":
		return runFeishuSetup(args[1:], in, out)
	case "help", "--help", "-h":
		printFeishuUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown feishu command: %s", strings.Join(args, " "))
	}
}

type feishuSetupOptions struct {
	Force            bool
	NoQR             bool
	ConfigPath       string
	Domain           string
	LarkDomain       string
	AllowedOpenIDs   string
	AllowedChatIDs   string
	DefaultCWD       string
	CodexChatsRoot   string
	CodexBin         string
	NotifyNewRun     string
	NotifySystem     string
	OpenCodexDesktop string
	RequestTimeout   time.Duration
	RegistrationHTTP *http.Client
}

func runFeishuSetup(args []string, in io.Reader, out io.Writer) error {
	for _, arg := range args {
		switch arg {
		case "help", "--help", "-h":
			printFeishuUsage(out)
			return nil
		}
	}
	opts, err := parseFeishuSetupOptions(args)
	if err != nil {
		return err
	}
	return runFeishuSetupWithOptions(opts, in, out)
}

func parseFeishuSetupOptions(args []string) (feishuSetupOptions, error) {
	opts := feishuSetupOptions{
		ConfigPath:     config.ConfigFilePath(),
		RequestTimeout: 10 * time.Minute,
	}
	fs := flagSet("ctr-go feishu setup")
	fs.BoolVar(&opts.Force, "force", false, "overwrite existing config.env")
	fs.BoolVar(&opts.NoQR, "no-qr", false, "print only the setup link, without a terminal QR code")
	fs.StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "config.env path")
	fs.StringVar(&opts.Domain, "domain", "", "Feishu accounts domain")
	fs.StringVar(&opts.LarkDomain, "lark-domain", "", "Lark accounts domain used after domain switch")
	fs.StringVar(&opts.AllowedOpenIDs, "feishu-allowed-open-ids", "", "allowed Feishu open ids")
	fs.StringVar(&opts.AllowedChatIDs, "feishu-allowed-chat-ids", "", "allowed Feishu chat ids")
	fs.StringVar(&opts.DefaultCWD, "default-cwd", "", "default Codex working directory")
	fs.StringVar(&opts.CodexChatsRoot, "codex-chats-root", "", "Codex UI Chats root")
	fs.StringVar(&opts.CodexBin, "codex-bin", "", "Codex binary path")
	fs.StringVar(&opts.NotifyNewRun, "notify-new-run", "", "notify on New run")
	fs.StringVar(&opts.NotifySystem, "notify-system", "", "send macOS system notifications for completion, failure, and approval")
	fs.StringVar(&opts.OpenCodexDesktop, "open-codex-desktop", "", "open Codex Desktop to the target thread after Feishu input")
	if err := fs.Parse(args); err != nil {
		return opts, fmt.Errorf("usage: ctr-go feishu setup [flags]")
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("usage: ctr-go feishu setup [flags]")
	}
	opts.ConfigPath = filepath.Clean(opts.ConfigPath)
	return opts, nil
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func runFeishuSetupWithOptions(opts feishuSetupOptions, in io.Reader, out io.Writer) error {
	existing, err := LoadEnvFileIfExists(opts.ConfigPath)
	if err != nil {
		return err
	}
	if len(existing) > 0 && !opts.Force {
		return fmt.Errorf("%s already exists; rerun with --force to overwrite it", opts.ConfigPath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.RequestTimeout)
	defer cancel()
	_, _ = fmt.Fprintln(out, "Feishu/Lark app setup")
	_, _ = fmt.Fprintln(out, "Scan the QR code with the Feishu/Lark mobile app, then approve app creation.")
	client := feishu.RegistrationClient{
		HTTPClient: opts.RegistrationHTTP,
		Domain:     opts.Domain,
		LarkDomain: opts.LarkDomain,
		Source:     "ctr-go-feishu-setup",
	}
	result, err := client.Register(ctx, feishu.RegistrationOptions{
		OnQRCodeReady: func(info feishu.RegistrationQRCode) {
			_, _ = fmt.Fprintf(out, "\nSetup link: %s\n", info.URL)
			if info.ExpireIn > 0 {
				_, _ = fmt.Fprintf(out, "Expires in: %s\n", info.ExpireIn.Round(time.Second))
			}
			if !opts.NoQR {
				if path, err := writeSetupQRCodePNG(info.URL); err == nil {
					_, _ = fmt.Fprintf(out, "QR image: %s\n", path)
					_, _ = fmt.Fprintf(out, "![Feishu setup QR](%s)\n", path)
				} else {
					_, _ = fmt.Fprintf(out, "Could not write QR image: %v\n", err)
				}
				_, _ = fmt.Fprintln(out)
				qrterminal.GenerateHalfBlock(info.URL, qrterminal.M, out)
			}
			_, _ = fmt.Fprintln(out, "\nWaiting for approval...")
		},
		OnStatusChange: func(status feishu.RegistrationStatus) {
			switch status.Status {
			case "slow_down":
				_, _ = fmt.Fprintf(out, "Still waiting; polling slowed to %s.\n", status.Interval.Round(time.Second))
			case "domain_switched":
				_, _ = fmt.Fprintln(out, "Detected Lark tenant; switched registration domain.")
			case "polling":
				_, _ = fmt.Fprint(out, ".")
			}
		},
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "\nApproved. Writing local config...")

	values, err := feishuSetupConfigValues(opts, existing, result)
	if err != nil {
		return err
	}
	if err := writeConfigEnv(opts.ConfigPath, values, opts.Force); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Wrote %s\n", opts.ConfigPath)
	_, _ = fmt.Fprintln(out, "\nSetup summary")
	_, _ = fmt.Fprintln(out, "  Adapter: feishu")
	_, _ = fmt.Fprintf(out, "  Feishu app id: %s\n", result.ClientID)
	_, _ = fmt.Fprintln(out, "  Feishu app secret: configured")
	if strings.TrimSpace(result.UserOpenID) != "" {
		_, _ = fmt.Fprintf(out, "  Creator open id: %s\n", result.UserOpenID)
	}
	if strings.TrimSpace(values["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"]) != "" {
		_, _ = fmt.Fprintf(out, "  Allowed Feishu open ids: %s\n", values["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"])
	} else {
		_, _ = fmt.Fprintln(out, "  Allowed Feishu open ids: any")
	}
	_, _ = fmt.Fprintln(out, "\nNext steps")
	_, _ = fmt.Fprintln(out, "  ctr-go doctor")
	_, _ = fmt.Fprintln(out, "  ctr-go daemon run")
	printFeishuWorkspaceNextSteps(out, "  ")
	return nil
}

func writeSetupQRCodePNG(setupURL string) (string, error) {
	code, err := qr.Encode(setupURL, qr.M)
	if err != nil {
		return "", err
	}
	path := filepath.Join(os.TempDir(), "codex-feishu-setup-qr.png")
	if err := os.WriteFile(path, code.PNG(), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func feishuSetupConfigValues(opts feishuSetupOptions, existing map[string]string, result feishu.RegistrationResult) (map[string]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	codexBin := "codex"
	if found, err := exec.LookPath("codex"); err == nil && strings.TrimSpace(found) != "" {
		codexBin = found
	}
	values := make(map[string]string, len(existing)+8)
	for key, value := range existing {
		values[key] = value
	}
	values["CTR_GO_ADAPTER"] = "feishu"
	values["CTR_GO_FEISHU_APP_ID"] = result.ClientID
	values["CTR_GO_FEISHU_APP_SECRET"] = result.ClientSecret
	values["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"] = strings.TrimSpace(firstNonEmpty(opts.AllowedOpenIDs, existing["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"], result.UserOpenID))
	values["CTR_GO_FEISHU_ALLOWED_CHAT_IDS"] = strings.TrimSpace(firstNonEmpty(opts.AllowedChatIDs, existing["CTR_GO_FEISHU_ALLOWED_CHAT_IDS"]))
	values["CTR_GO_DEFAULT_CWD"] = strings.TrimSpace(firstNonEmpty(opts.DefaultCWD, existing["CTR_GO_DEFAULT_CWD"], cwd))
	values["CTR_GO_CODEX_CHATS_ROOT"] = strings.TrimSpace(firstNonEmpty(opts.CodexChatsRoot, existing["CTR_GO_CODEX_CHATS_ROOT"], config.DefaultCodexChatsRoot()))
	values["CTR_GO_CODEX_BIN"] = strings.TrimSpace(firstNonEmpty(opts.CodexBin, existing["CTR_GO_CODEX_BIN"], codexBin))
	values["CTR_GO_NOTIFY_NEW_RUN"] = strings.TrimSpace(firstNonEmpty(opts.NotifyNewRun, existing["CTR_GO_NOTIFY_NEW_RUN"], "true"))
	values["CTR_GO_NOTIFY_SYSTEM"] = strings.TrimSpace(firstNonEmpty(opts.NotifySystem, existing["CTR_GO_NOTIFY_SYSTEM"], "false"))
	values["CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU"] = strings.TrimSpace(firstNonEmpty(opts.OpenCodexDesktop, existing["CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU"], "true"))
	for _, key := range config.RuntimeEnvPassthroughKeys() {
		if strings.TrimSpace(values[key]) != "" {
			continue
		}
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			values[key] = value
		}
	}
	if strings.TrimSpace(values["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"]) == "" {
		delete(values, "CTR_GO_FEISHU_ALLOWED_OPEN_IDS")
	}
	if strings.TrimSpace(values["CTR_GO_FEISHU_ALLOWED_CHAT_IDS"]) == "" {
		delete(values, "CTR_GO_FEISHU_ALLOWED_CHAT_IDS")
	}
	return validateFeishuSetupValues(values)
}

func validateFeishuSetupValues(values map[string]string) (map[string]string, error) {
	required := []string{
		"CTR_GO_ADAPTER",
		"CTR_GO_FEISHU_APP_ID",
		"CTR_GO_FEISHU_APP_SECRET",
		"CTR_GO_DEFAULT_CWD",
		"CTR_GO_CODEX_CHATS_ROOT",
		"CTR_GO_CODEX_BIN",
		"CTR_GO_NOTIFY_NEW_RUN",
		"CTR_GO_NOTIFY_SYSTEM",
		"CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU",
	}
	for _, key := range required {
		if strings.TrimSpace(values[key]) == "" {
			return nil, fmt.Errorf("%s is required", key)
		}
	}
	if err := validateDirectory(values["CTR_GO_DEFAULT_CWD"]); err != nil {
		return nil, fmt.Errorf("CTR_GO_DEFAULT_CWD: %w", err)
	}
	if err := validateExecutableRef(values["CTR_GO_CODEX_BIN"]); err != nil {
		return nil, fmt.Errorf("CTR_GO_CODEX_BIN: %w", err)
	}
	if err := validateBoolText(values["CTR_GO_NOTIFY_NEW_RUN"]); err != nil {
		return nil, fmt.Errorf("CTR_GO_NOTIFY_NEW_RUN: %w", err)
	}
	if err := validateBoolText(values["CTR_GO_NOTIFY_SYSTEM"]); err != nil {
		return nil, fmt.Errorf("CTR_GO_NOTIFY_SYSTEM: %w", err)
	}
	if err := validateBoolText(values["CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU"]); err != nil {
		return nil, fmt.Errorf("CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU: %w", err)
	}
	return values, nil
}

func runDaemon(cfg config.Config) error {
	adapter := selectedAdapter(cfg)
	if adapter == "" {
		return errors.New("configure Feishu credentials: CTR_GO_FEISHU_APP_ID and CTR_GO_FEISHU_APP_SECRET are required; run ctr-go feishu setup")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := daemonLogger(cfg)
	service, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	defer service.Close()
	service.SetLogger(diagnosticLogger(cfg, logger))

	bot, err := newAdapterBot(cfg, service, logger, adapter)
	if err != nil {
		return err
	}
	service.SetSender(bot)
	if err := service.Start(ctx); err != nil {
		return err
	}
	if err := bot.Start(ctx); err != nil {
		return err
	}
	logger.Printf("ctr-go daemon running with %s", bot.String())
	return bot.Run(ctx)
}

type adapterBot interface {
	daemon.Sender
	Start(context.Context) error
	Run(context.Context) error
	String() string
}

func newAdapterBot(cfg config.Config, service *daemon.Service, logger *log.Logger, adapter string) (adapterBot, error) {
	switch adapter {
	case "feishu":
		return feishu.NewBot(cfg, service, service.Store(), logger)
	default:
		return nil, fmt.Errorf("unsupported adapter: %s", adapter)
	}
}

func selectedAdapter(cfg config.Config) string {
	switch strings.TrimSpace(strings.ToLower(cfg.Adapter)) {
	case "", "auto", "feishu", "lark":
		return "feishu"
	}
	if strings.TrimSpace(cfg.FeishuAppID) != "" && strings.TrimSpace(cfg.FeishuAppSecret) != "" {
		return "feishu"
	}
	return ""
}

func daemonLogger(cfg config.Config) *log.Logger {
	return log.New(daemonLogOutput(cfg), "", log.LstdFlags)
}

func daemonLogOutput(cfg config.Config) io.Writer {
	if !cfg.LogEnabled {
		return io.Discard
	}
	return os.Stdout
}

func diagnosticLogger(cfg config.Config, logger *log.Logger) *log.Logger {
	if !cfg.LogEnabled || !cfg.DiagnosticLogs {
		return nil
	}
	return logger
}

func runStatus(cfg config.Config, out io.Writer) error {
	service, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	defer service.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	doctor, err := service.Doctor(ctx)
	if err != nil {
		return err
	}
	state := normalizeStringMap(doctor["daemon_state"])
	backlog := normalizeInt64(doctor["delivery_backlog"])

	lines := []string{
		"ctr-go status",
		fmt.Sprintf("Home: %s", cfg.Paths.Home),
		fmt.Sprintf("DB: %s", cfg.Paths.DBPath),
		fmt.Sprintf("Codex bin: %s", cfg.CodexBin),
		fmt.Sprintf("Adapter: %s", selectedAdapterLabel(cfg)),
		fmt.Sprintf("Feishu configured: %t", strings.TrimSpace(cfg.FeishuAppID) != "" && strings.TrimSpace(cfg.FeishuAppSecret) != ""),
		fmt.Sprintf("Allowed Feishu open ids: %s", formatStrings(cfg.FeishuAllowedOpenIDs)),
		fmt.Sprintf("Allowed Feishu chats: %s", formatStrings(cfg.FeishuAllowedChatIDs)),
		fmt.Sprintf("Default cwd: %s", cfg.DefaultCWD),
		fmt.Sprintf("Codex Chats root: %s", cfg.CodexChatsRoot),
		fmt.Sprintf("Delivery backlog: %d", backlog),
		"",
		"Persisted daemon state:",
	}
	if len(state) == 0 {
		lines = append(lines, "(empty)")
	} else {
		keys := make([]string, 0, len(state))
		for key := range state {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = append(lines, fmt.Sprintf("%s = %s", key, state[key]))
		}
	}
	_, _ = fmt.Fprintln(out, strings.Join(lines, "\n"))
	return nil
}

func selectedAdapterLabel(cfg config.Config) string {
	adapter := selectedAdapter(cfg)
	if adapter == "" {
		return "(not configured)"
	}
	return adapter
}

func runDoctor(cfg config.Config, out io.Writer) error {
	service, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	defer service.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	doctor, err := service.Doctor(ctx)
	if err != nil {
		return err
	}
	state := normalizeStringMap(doctor["daemon_state"])
	doctor["health"] = runHealthChecks(ctx, cfg, state)
	encoded, err := json.MarshalIndent(doctor, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, string(encoded))
	return nil
}

func runRepair(cfg config.Config, out io.Writer) error {
	if serviceGOOS == "darwin" && serviceIsLoaded(context.Background(), launchctlTarget()) {
		if err := runServiceRestart(out); err == nil {
			_, _ = fmt.Fprintln(out, "Repair completed by restarting codex-feishu service.")
			return nil
		} else {
			_, _ = fmt.Fprintf(out, "Service restart unavailable, falling back to in-daemon repair: %v\n", err)
		}
	}
	service, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	defer service.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.RequestRepair(ctx, "cli"); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "Repair requested.")
	return nil
}

func runInit(args []string, in io.Reader, out io.Writer) error {
	force := false
	for _, arg := range args {
		switch arg {
		case "--force":
			force = true
		case "help", "--help", "-h":
			printInitUsage(out)
			return nil
		default:
			return fmt.Errorf("usage: ctr-go init [--force]")
		}
	}

	path := config.ConfigFilePath()
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; rerun with --force to overwrite it", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	codexBin := "codex"
	if found, err := exec.LookPath("codex"); err == nil && strings.TrimSpace(found) != "" {
		codexBin = found
	}

	reader := bufio.NewReader(in)
	_, _ = fmt.Fprintln(out, "This writes a local codex-feishu config file. Keep it private.")
	values := map[string]string{
		"CTR_GO_ADAPTER": "feishu",
	}
	appID, err := promptRequired(reader, out, "Feishu app id")
	if err != nil {
		return err
	}
	appSecret, err := promptRequired(reader, out, "Feishu app secret")
	if err != nil {
		return err
	}
	allowedOpenIDs, err := prompt(reader, out, "Allowed Feishu open id(s), optional", "")
	if err != nil {
		return err
	}
	allowedChats, err := prompt(reader, out, "Allowed Feishu chat id(s), optional", "")
	if err != nil {
		return err
	}
	values["CTR_GO_FEISHU_APP_ID"] = appID
	values["CTR_GO_FEISHU_APP_SECRET"] = appSecret
	if strings.TrimSpace(allowedOpenIDs) != "" {
		values["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"] = allowedOpenIDs
	}
	if strings.TrimSpace(allowedChats) != "" {
		values["CTR_GO_FEISHU_ALLOWED_CHAT_IDS"] = allowedChats
	}
	defaultCWD, err := prompt(reader, out, "Default cwd", cwd)
	if err != nil {
		return err
	}
	chatsRoot, err := prompt(reader, out, "Codex Chats root", config.DefaultCodexChatsRoot())
	if err != nil {
		return err
	}
	selectedCodexBin, err := prompt(reader, out, "Codex binary", codexBin)
	if err != nil {
		return err
	}
	notifyNewRun, err := prompt(reader, out, "Notify on New run", "true")
	if err != nil {
		return err
	}
	notifySystem, err := prompt(reader, out, "macOS system notifications (removed)", "false")
	if err != nil {
		return err
	}

	values["CTR_GO_DEFAULT_CWD"] = defaultCWD
	values["CTR_GO_CODEX_CHATS_ROOT"] = chatsRoot
	values["CTR_GO_CODEX_BIN"] = selectedCodexBin
	values["CTR_GO_NOTIFY_NEW_RUN"] = notifyNewRun
	values["CTR_GO_NOTIFY_SYSTEM"] = notifySystem
	if err := writeConfigEnv(path, values, force); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "\nWrote %s\n", path)
	_, _ = fmt.Fprintln(out, "Next steps:")
	_, _ = fmt.Fprintln(out, "  ctr-go doctor")
	_, _ = fmt.Fprintln(out, "  ctr-go daemon run")
	printFeishuWorkspaceNextSteps(out, "  ")
	return nil
}

func printFeishuWorkspaceNextSteps(out io.Writer, prefix string) {
	_, _ = fmt.Fprintln(out, prefix+"Open Feishu/Lark and send /start to the Codex bot.")
	_, _ = fmt.Fprintln(out, prefix+"Use the Codex bot DM as the daily workspace.")
	_, _ = fmt.Fprintln(out, prefix+"Each Codex thread opens as a topic reply; reply in that topic to continue remote control.")
}

func promptRequired(reader *bufio.Reader, out io.Writer, label string) (string, error) {
	value, err := prompt(reader, out, label, "")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return value, nil
}

func prompt(reader *bufio.Reader, out io.Writer, label, fallback string) (string, error) {
	if fallback == "" {
		_, _ = fmt.Fprintf(out, "%s: ", label)
	} else {
		_, _ = fmt.Fprintf(out, "%s [%s]: ", label, fallback)
	}
	line, err := reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return fallback, nil
	}
	return value, nil
}

func writeConfigEnv(path string, values map[string]string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	_, _ = fmt.Fprintln(file, "# codex-feishu local configuration")
	_, _ = fmt.Fprintln(file, "# Keep this file private. It contains bot credentials.")
	for _, key := range keys {
		_, _ = fmt.Fprintf(file, "%s=%s\n", key, strconv.Quote(values[key]))
	}
	return file.Chmod(0o600)
}

func printUsage(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Usage:")
	_, _ = fmt.Fprintln(out, "  ctr-go init [--force]")
	_, _ = fmt.Fprintln(out, "  ctr-go feishu setup [--force] [flags]")
	_, _ = fmt.Fprintln(out, "  ctr-go service install [flags]")
	_, _ = fmt.Fprintln(out, "  ctr-go service start|stop|restart|status|enable-login|disable-login|uninstall")
	_, _ = fmt.Fprintln(out, "  ctr-go daemon run")
	_, _ = fmt.Fprintln(out, "  ctr-go status")
	_, _ = fmt.Fprintln(out, "  ctr-go doctor")
	_, _ = fmt.Fprintln(out, "  ctr-go repair")
	_, _ = fmt.Fprintln(out, "  ctr-go version")
}

func printInitUsage(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Usage:")
	_, _ = fmt.Fprintln(out, "  ctr-go init [--force]")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Creates a private local config.env for codex-feishu.")
	_, _ = fmt.Fprintln(out, "Set CTR_GO_CONFIG to choose a custom config path.")
}

func printFeishuUsage(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Usage:")
	_, _ = fmt.Fprintln(out, "  ctr-go feishu setup [--force] [--no-qr] [flags]")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Creates a Feishu/Lark app with the official scan-to-create flow and writes local config.env.")
	_, _ = fmt.Fprintln(out, "Set CTR_GO_CONFIG or pass --config to choose a custom config path.")
}

func formatIDs(values []int64) string {
	if len(values) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("%d", value))
	}
	return strings.Join(parts, ", ")
}

func formatStrings(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ", ")
}

func normalizeStringMap(value any) map[string]string {
	switch typed := value.(type) {
	case map[string]string:
		return typed
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, raw := range typed {
			out[key] = fmt.Sprintf("%v", raw)
		}
		return out
	default:
		return nil
	}
}

func normalizeInt64(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	default:
		return 0
	}
}
