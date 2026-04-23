package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is persisted to disk as YAML. Secrets can also be read from
// environment variables to avoid storing them in plaintext.
type Config struct {
	IMAP   IMAPConfig   `yaml:"imap"`
	LLM    LLMConfig    `yaml:"llm"`
	Rules  RulesConfig  `yaml:"rules"`
	Policy PolicyConfig `yaml:"policy"`
	Paths  PathsConfig  `yaml:"paths"`
}

type IMAPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	// Password: literal password, or "env:VAR_NAME" to read from environment.
	Password string `yaml:"password"`
	UseTLS   bool   `yaml:"tls"`
	// Inbox to scan — usually INBOX.
	Inbox string `yaml:"inbox"`
	// ArchiveRoot is the parent folder under which smartmail creates category
	// folders. Empty means top-level. Gmail users usually want "" because
	// Gmail treats labels as folders.
	ArchiveRoot string `yaml:"archive_root"`
}

type LLMConfig struct {
	// Provider: "openai" or "lmstudio" (any OpenAI-compatible endpoint).
	Provider string `yaml:"provider"`
	// BaseURL overrides the default. For LMStudio use http://localhost:1234/v1
	BaseURL string `yaml:"base_url"`
	// APIKey: literal, or "env:VAR_NAME". LMStudio accepts any value.
	APIKey string `yaml:"api_key"`
	Model  string `yaml:"model"`
	// Temperature for classification calls. 0 is most deterministic.
	Temperature float64 `yaml:"temperature"`
	// MaxTokens caps the response length.
	MaxTokens int `yaml:"max_tokens"`
	// Timeout in seconds per request.
	TimeoutSec int `yaml:"timeout_sec"`
}

type RulesConfig struct {
	// TrustedSenders skip LLM classification entirely and go to the inbox.
	TrustedSenders []string `yaml:"trusted_senders"`
	// BlockedSenders skip LLM and go straight to spam.
	BlockedSenders []string `yaml:"blocked_senders"`
	// PinnedFolders are preferred folder names the LLM should reuse.
	PinnedFolders []string `yaml:"pinned_folders"`
}

type PolicyConfig struct {
	// MinConfidence below which we leave the message in the inbox rather
	// than risk a wrong move. 0..1.
	MinConfidence float64 `yaml:"min_confidence"`
	// MaxBodyChars truncates the email body before sending to the LLM to
	// control token cost.
	MaxBodyChars int `yaml:"max_body_chars"`
	// MarkSeen: mark messages as read after moving.
	MarkSeen bool `yaml:"mark_seen"`
	// Concurrency for parallel LLM classifications.
	Concurrency int `yaml:"concurrency"`
}

type PathsConfig struct {
	// StateFile tracks processed UIDs so we never reprocess a message.
	StateFile string `yaml:"state_file"`
	// AuditFile is an append-only log of every move, for undo.
	AuditFile string `yaml:"audit_file"`
}

// Resolve expands env:VAR_NAME references. Call after loading.
func (c *Config) Resolve() error {
	if v, ok := resolveEnv(c.IMAP.Password); ok {
		c.IMAP.Password = v
	} else {
		return fmt.Errorf("imap.password: env var %q is empty", strings.TrimPrefix(c.IMAP.Password, "env:"))
	}
	if v, ok := resolveEnv(c.LLM.APIKey); ok {
		c.LLM.APIKey = v
	}
	// Default paths relative to config dir.
	if c.Paths.StateFile == "" {
		c.Paths.StateFile = "smartmail.state.json"
	}
	if c.Paths.AuditFile == "" {
		c.Paths.AuditFile = "smartmail.audit.log"
	}
	// Defaults.
	if c.LLM.Temperature == 0 && c.LLM.Provider == "" {
		// only override when fully unset — Temperature=0 is a valid choice
	}
	if c.LLM.MaxTokens == 0 {
		c.LLM.MaxTokens = 800
	}
	if c.LLM.TimeoutSec == 0 {
		c.LLM.TimeoutSec = 60
	}
	if c.Policy.MinConfidence == 0 {
		c.Policy.MinConfidence = 0.6
	}
	if c.Policy.MaxBodyChars == 0 {
		c.Policy.MaxBodyChars = 4000
	}
	if c.Policy.Concurrency == 0 {
		c.Policy.Concurrency = 3
	}
	if c.IMAP.Inbox == "" {
		c.IMAP.Inbox = "INBOX"
	}
	if c.IMAP.Port == 0 {
		if c.IMAP.UseTLS {
			c.IMAP.Port = 993
		} else {
			c.IMAP.Port = 143
		}
	}
	return nil
}

func resolveEnv(val string) (string, bool) {
	if !strings.HasPrefix(val, "env:") {
		return val, val != ""
	}
	v := os.Getenv(strings.TrimPrefix(val, "env:"))
	return v, v != ""
}

func LoadConfig(path string) (*Config, string, error) {
	if path == "" {
		// Discovery: ./smartmail.yaml → $XDG_CONFIG_HOME/smartmail/config.yaml → ~/.config/smartmail/config.yaml
		candidates := []string{"smartmail.yaml", "smartmail.yml"}
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			candidates = append(candidates, filepath.Join(xdg, "smartmail", "config.yaml"))
		}
		if home, _ := os.UserHomeDir(); home != "" {
			candidates = append(candidates, filepath.Join(home, ".config", "smartmail", "config.yaml"))
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}
	if path == "" {
		return nil, "", fmt.Errorf("no config found — run 'smartmail init' first")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, path, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Resolve(); err != nil {
		return nil, path, err
	}
	// Resolve relative state/audit paths against the config directory.
	cfgDir := filepath.Dir(path)
	if !filepath.IsAbs(c.Paths.StateFile) {
		c.Paths.StateFile = filepath.Join(cfgDir, c.Paths.StateFile)
	}
	if !filepath.IsAbs(c.Paths.AuditFile) {
		c.Paths.AuditFile = filepath.Join(cfgDir, c.Paths.AuditFile)
	}
	return &c, path, nil
}

