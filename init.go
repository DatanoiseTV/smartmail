package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// presetServers is a short list of well-known providers so users don't have
// to hunt for hostnames. Extend freely — the "custom" option always works.
var presetServers = []struct {
	Label      string
	Host       string
	Port       int
	TLS        bool
	Hint       string
	ArchiveTip string
}{
	{"Gmail / Google Workspace", "imap.gmail.com", 993, true, "requires an App Password — https://myaccount.google.com/apppasswords", "[Gmail]/All Mail"},
	{"iCloud Mail", "imap.mail.me.com", 993, true, "requires an app-specific password — https://appleid.apple.com", ""},
	{"Fastmail", "imap.fastmail.com", 993, true, "use an app password from Fastmail settings", ""},
	{"Outlook / Microsoft 365", "outlook.office365.com", 993, true, "OAuth is preferred; basic auth may require an app password", ""},
	{"Yahoo Mail", "imap.mail.yahoo.com", 993, true, "requires an app password", ""},
	{"ProtonMail (Bridge)", "127.0.0.1", 1143, false, "Proton Bridge must be running locally", ""},
	{"Custom / self-hosted", "", 993, true, "", ""},
}

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	out := fs.String("out", "smartmail.yaml", "output config path")
	force := fs.Bool("force", false, "overwrite existing config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ui.Banner()
	ui.Section("Interactive setup")

	if _, err := os.Stat(*out); err == nil && !*force {
		if ui.Prompt(fmt.Sprintf("%s already exists. Overwrite? (y/N)", *out), "n") != "y" {
			return fmt.Errorf("aborted")
		}
	}

	cfg := &Config{}

	// --- IMAP ---
	ui.Info("Pick your mail provider:")
	for i, p := range presetServers {
		fmt.Printf("  %d) %s\n", i+1, p.Label)
	}
	pick, _ := strconv.Atoi(ui.Prompt("choice", "1"))
	if pick < 1 || pick > len(presetServers) {
		pick = 1
	}
	preset := presetServers[pick-1]
	if preset.Hint != "" {
		ui.Dim(preset.Hint)
	}

	if preset.Host == "" {
		cfg.IMAP.Host = ui.Prompt("IMAP host", "")
		portStr := ui.Prompt("IMAP port", "993")
		cfg.IMAP.Port, _ = strconv.Atoi(portStr)
		cfg.IMAP.UseTLS = strings.ToLower(ui.Prompt("Use TLS? (y/n)", "y")) == "y"
	} else {
		cfg.IMAP.Host = preset.Host
		cfg.IMAP.Port = preset.Port
		cfg.IMAP.UseTLS = preset.TLS
	}

	cfg.IMAP.Username = ui.Prompt("Username (usually your full email)", "")

	fmt.Printf("  %s password (hidden — press enter): ", "?")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	storePassword := ui.Prompt("Store password in config file or read from env var? (file/env)", "env")
	if strings.HasPrefix(storePassword, "e") {
		envVar := ui.Prompt("Env var name", "SMARTMAIL_IMAP_PASSWORD")
		cfg.IMAP.Password = "env:" + envVar
		ui.Dim("remember to: export %s='...'", envVar)
	} else {
		cfg.IMAP.Password = string(pw)
	}

	cfg.IMAP.Inbox = ui.Prompt("Inbox folder", "INBOX")
	cfg.IMAP.ArchiveRoot = ui.Prompt("Parent folder for categories (empty for top-level)", preset.ArchiveTip)

	// Connectivity test — don't write a broken config.
	ui.Info("Testing IMAP connection...")
	testPw := cfg.IMAP.Password
	if strings.HasPrefix(testPw, "env:") {
		testPw = string(pw)
	}
	if err := testIMAP(cfg.IMAP, testPw); err != nil {
		ui.Warn("IMAP test failed: %v", err)
		if ui.Prompt("Save config anyway? (y/N)", "n") != "y" {
			return fmt.Errorf("aborted")
		}
	} else {
		ui.OK("IMAP connection successful")
	}

	// --- LLM ---
	ui.Section("LLM provider")
	fmt.Println("  1) OpenAI (remote, best quality)")
	fmt.Println("  2) LMStudio (local, private, free)")
	fmt.Println("  3) Custom OpenAI-compatible endpoint")
	llmPick, _ := strconv.Atoi(ui.Prompt("choice", "1"))
	switch llmPick {
	case 2:
		cfg.LLM.Provider = "lmstudio"
		cfg.LLM.BaseURL = ui.Prompt("LMStudio base URL", "http://localhost:1234/v1")
		cfg.LLM.Model = ui.Prompt("Model name (as loaded in LMStudio)", "local-model")
		cfg.LLM.APIKey = "lm-studio"
	case 3:
		cfg.LLM.Provider = "openai"
		cfg.LLM.BaseURL = ui.Prompt("Base URL", "")
		cfg.LLM.Model = ui.Prompt("Model", "")
		envVar := ui.Prompt("Env var with API key", "SMARTMAIL_LLM_KEY")
		cfg.LLM.APIKey = "env:" + envVar
	default:
		cfg.LLM.Provider = "openai"
		cfg.LLM.BaseURL = "https://api.openai.com/v1"
		cfg.LLM.Model = ui.Prompt("Model", "gpt-4o-mini")
		envVar := ui.Prompt("Env var with OpenAI API key", "OPENAI_API_KEY")
		cfg.LLM.APIKey = "env:" + envVar
	}
	cfg.LLM.Temperature = 0
	cfg.LLM.MaxTokens = 800
	cfg.LLM.TimeoutSec = 60

	// --- Policy ---
	ui.Section("Policy")
	minC := ui.Prompt("Minimum confidence to move a message (0-1)", "0.65")
	cfg.Policy.MinConfidence, _ = strconv.ParseFloat(minC, 64)
	cfg.Policy.MaxBodyChars = 4000
	cfg.Policy.MarkSeen = strings.ToLower(ui.Prompt("Mark moved messages as read? (y/n)", "n")) == "y"
	conc := ui.Prompt("Concurrent classifications", "3")
	cfg.Policy.Concurrency, _ = strconv.Atoi(conc)

	// Defaults for rules/pinned folders.
	cfg.Rules.PinnedFolders = []string{
		"Personal", "Work", "Finance", "Receipts", "Shopping", "Travel",
		"Newsletters", "Marketing", "Social", "Notifications", "Updates",
	}

	cfg.Paths.StateFile = "smartmail.state.json"
	cfg.Paths.AuditFile = "smartmail.audit.log"

	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	if err := os.WriteFile(*out, b, 0o600); err != nil {
		return err
	}
	ui.OK("Wrote %s", *out)

	ui.Section("Next steps")
	fmt.Println("  1. Ensure the required env vars are exported in your shell.")
	fmt.Println("  2. Dry-run first:   smartmail run --dry-run --limit 20 --verbose")
	fmt.Println("  3. When happy:      smartmail run")
	fmt.Println("  4. For live sorting: smartmail watch")
	_ = ctx
	return nil
}

func testIMAP(cfg IMAPConfig, password string) error {
	var tlsCfg *tls.Config
	if cfg.UseTLS {
		tlsCfg = &tls.Config{ServerName: cfg.Host}
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	cli, err := dialIMAP(addr, tlsCfg, cfg.UseTLS)
	if err != nil {
		return err
	}
	defer cli.Close()
	return cli.Login(cfg.Username, password)
}
