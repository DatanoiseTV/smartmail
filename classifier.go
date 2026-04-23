package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

// Classifier is the runtime glue between IMAP, the LLM, and the audit/state
// layers. One instance per run/watch invocation.
type Classifier struct {
	cfg     *Config
	imap    *Client
	llm     *LLM
	state   *State
	audit   *Audit
	dry     bool
	verbose bool

	spamFolder     string   // resolved once, e.g. "Spam" or "[Gmail]/Spam"
	existingCache  []string // cached mailbox list
	cacheFetchedAt time.Time
}

func NewClassifier(cfg *Config, imapCli *Client, llm *LLM, state *State, audit *Audit, dry, verbose bool) *Classifier {
	return &Classifier{
		cfg: cfg, imap: imapCli, llm: llm,
		state: state, audit: audit,
		dry: dry, verbose: verbose,
	}
}

// Prepare resolves the spam folder and caches the mailbox list. Must be
// called after the inbox has been selected.
func (c *Classifier) Prepare() error {
	folders, err := c.imap.ListMailboxes()
	if err != nil {
		return fmt.Errorf("list mailboxes: %w", err)
	}
	c.existingCache = folders
	c.cacheFetchedAt = time.Now()

	// Heuristic spam folder detection — providers disagree.
	candidates := []string{"[Gmail]/Spam", "Spam", "Junk", "Junk E-mail", "Junk Email", "INBOX.Spam", "INBOX.Junk"}
	for _, cand := range candidates {
		for _, f := range folders {
			if strings.EqualFold(f, cand) {
				c.spamFolder = f
				return nil
			}
		}
	}
	c.spamFolder = "Spam"
	return nil
}

// Process classifies every given message, in parallel up to cfg.Policy.Concurrency.
func (c *Classifier) Process(ctx context.Context, msgs []*Message) (stats ProcessStats) {
	sem := make(chan struct{}, max1(c.cfg.Policy.Concurrency))
	results := make(chan ProcessStats, len(msgs))

	for _, m := range msgs {
		m := m
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			s := c.processOne(ctx, m)
			results <- s
		}()
	}
	// Drain.
	for i := 0; i < len(msgs); i++ {
		select {
		case s := <-results:
			stats.merge(s)
		case <-ctx.Done():
			stats.Cancelled++
		}
	}
	return
}

func (c *Classifier) processOne(ctx context.Context, m *Message) ProcessStats {
	var s ProcessStats
	s.Total = 1

	// Deterministic pre-filters before spending LLM tokens.
	if decision, matched := c.preFilter(m); matched {
		c.apply(m, decision)
		return tallyDecision(s, decision)
	}

	dec, err := c.askLLM(ctx, m)
	if err != nil {
		ui.Warn("uid %d LLM error: %v — keeping in inbox", m.UID, err)
		s.Errors++
		return s
	}

	// Confidence gate — never move something the model isn't sure about.
	if (dec.Tool == "file_email" || dec.Tool == "mark_spam") && dec.Confidence < c.cfg.Policy.MinConfidence {
		ui.Dim("uid %d low-confidence (%.2f < %.2f) — keeping in inbox: %s",
			m.UID, dec.Confidence, c.cfg.Policy.MinConfidence, dec.Reasoning)
		dec = &Decision{Tool: "keep_in_inbox", Reasoning: "below confidence threshold: " + dec.Reasoning}
	}

	c.apply(m, dec)
	return tallyDecision(s, dec)
}

func (c *Classifier) preFilter(m *Message) (*Decision, bool) {
	fromLower := strings.ToLower(m.From)
	for _, t := range c.cfg.Rules.TrustedSenders {
		if t != "" && strings.Contains(fromLower, strings.ToLower(t)) {
			return &Decision{Tool: "keep_in_inbox", Reasoning: "trusted sender rule matched: " + t, Priority: "normal"}, true
		}
	}
	for _, b := range c.cfg.Rules.BlockedSenders {
		if b != "" && strings.Contains(fromLower, strings.ToLower(b)) {
			return &Decision{Tool: "mark_spam", Reasoning: "blocked sender rule matched: " + b, Confidence: 1}, true
		}
	}
	return nil, false
}

func (c *Classifier) askLLM(ctx context.Context, m *Message) (*Decision, error) {
	tools := defineTools(c.cfg.Rules.PinnedFolders, c.existingCache)
	msgs := []chatMessage{
		{Role: "system", Content: c.systemPrompt()},
		{Role: "user", Content: renderEmailForLLM(m, c.cfg.Policy.MaxBodyChars)},
	}
	resp, err := c.llm.Call(ctx, msgs, tools)
	if err != nil {
		return nil, err
	}
	if len(resp.ToolCalls) == 0 {
		return nil, fmt.Errorf("model did not call any tool; content=%q", resp.Content)
	}
	call := resp.ToolCalls[0]
	var raw map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &raw); err != nil {
		return nil, fmt.Errorf("bad tool args: %w; raw=%s", err, call.Function.Arguments)
	}
	return parseDecision(call.Function.Name, raw), nil
}

func (c *Classifier) systemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are smartmail, an email-triage assistant. ")
	sb.WriteString("You decide what to do with a single email by calling exactly one of the provided tools. ")
	sb.WriteString("Goals, in order of priority:\n")
	sb.WriteString("  1. Never hide important mail. When in doubt, keep_in_inbox.\n")
	sb.WriteString("  2. Keep the folder tree tidy — prefer reusing existing folders over inventing new ones.\n")
	sb.WriteString("  3. Tag semantically so later search works across folders.\n")
	sb.WriteString("  4. Only mark_spam for clear spam/phishing/scams, never for legitimate subscribed marketing.\n\n")

	sb.WriteString("Heuristics you should weigh:\n")
	sb.WriteString("  • A List-Id or List-Unsubscribe header means this is a mailing list — newsletters or marketing.\n")
	sb.WriteString("  • Order/shipping confirmations, invoices, receipts → Receipts or Shopping with should_star=true when the amount matters.\n")
	sb.WriteString("  • Security-sensitive mail (password reset, 2FA code, suspicious login, billing failure) → keep_in_inbox with priority=high.\n")
	sb.WriteString("  • Direct human-written replies (no List-Id, personal tone, asks a question) → keep_in_inbox.\n")
	sb.WriteString("  • GitHub/Jira/Linear/CI notifications → Notifications, or Updates, tagged with the project.\n\n")

	if len(c.cfg.Rules.PinnedFolders) > 0 {
		sb.WriteString("Preferred categories (reuse these): ")
		sb.WriteString(strings.Join(c.cfg.Rules.PinnedFolders, ", "))
		sb.WriteString("\n")
	}
	if len(c.existingCache) > 0 {
		sb.WriteString("Existing mailboxes on the server (you may reuse any, but only reuse ones that semantically fit):\n")
		// cap to something reasonable for the prompt
		shown := c.existingCache
		if len(shown) > 60 {
			shown = shown[:60]
		}
		for _, f := range shown {
			sb.WriteString("  - ")
			sb.WriteString(f)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func renderEmailForLLM(m *Message, maxBody int) string {
	var sb strings.Builder
	sb.WriteString("FROM: ")
	sb.WriteString(m.From)
	sb.WriteString("\nTO: ")
	sb.WriteString(m.To)
	if m.Cc != "" {
		sb.WriteString("\nCC: ")
		sb.WriteString(m.Cc)
	}
	sb.WriteString("\nSUBJECT: ")
	sb.WriteString(m.Subject)
	sb.WriteString("\nDATE: ")
	sb.WriteString(m.Date.Format(time.RFC3339))
	if m.ListID != "" {
		sb.WriteString("\nLIST-ID: ")
		sb.WriteString(m.ListID)
	}
	if m.Unsubscribe != "" {
		sb.WriteString("\nLIST-UNSUBSCRIBE: present")
	}
	sb.WriteString("\nSIZE: ")
	sb.WriteString(fmt.Sprintf("%d bytes", m.SizeBytes))
	sb.WriteString("\n\n--- BODY ---\n")
	body := m.BodyText
	if len(body) > maxBody {
		body = body[:maxBody] + "…[truncated]"
	}
	sb.WriteString(body)
	return sb.String()
}

func parseDecision(toolName string, raw map[string]any) *Decision {
	d := &Decision{Tool: toolName}
	getStr := func(k string) string {
		if v, ok := raw[k].(string); ok {
			return v
		}
		return ""
	}
	getBool := func(k string) bool {
		if v, ok := raw[k].(bool); ok {
			return v
		}
		return false
	}
	getFloat := func(k string) float64 {
		switch v := raw[k].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		}
		return 0
	}
	getStrs := func(k string) []string {
		if v, ok := raw[k].([]any); ok {
			out := make([]string, 0, len(v))
			for _, e := range v {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		return nil
	}
	d.Reasoning = getStr("reasoning")
	d.Confidence = getFloat("confidence")
	d.Priority = getStr("priority")
	d.Tags = getStrs("tags")
	switch toolName {
	case "file_email":
		d.Category = getStr("category")
		d.Subfolder = getStr("subfolder")
		d.Star = getBool("should_star")
	case "keep_in_inbox":
		d.Star = getBool("should_flag")
	case "mark_spam":
		d.Indicators = getStrs("indicators")
	}
	return d
}

// apply turns a Decision into IMAP operations. All writes go through audit.
func (c *Classifier) apply(m *Message, d *Decision) {
	uids := []uint32{m.UID}

	// Always record the action — even dry runs — so the verbose log is useful.
	action := d.Tool
	switch d.Tool {
	case "file_email":
		target := c.sanitizeFolder(d.Category, d.Subfolder)
		if c.dry {
			ui.Decision(m.Subject, m.From, "file", target, d.Reasoning, true)
			return
		}
		created, err := c.imap.EnsureMailbox(target)
		if err != nil {
			ui.Warn("uid %d could not ensure folder %q: %v — keeping in inbox", m.UID, target, err)
			return
		}
		if err := c.applyTagsAndFlags(uids, d); err != nil {
			ui.Warn("uid %d flag/tag error: %v", m.UID, err)
		}
		if err := c.imap.Move(uids, created); err != nil {
			ui.Warn("uid %d move failed: %v", m.UID, err)
			return
		}
		if c.cfg.Policy.MarkSeen {
			// After move the UID belongs to the target mailbox; best-effort only.
			_ = c.imap.AddFlags(uids, imap.FlagSeen)
		}
		c.audit.Append(AuditEntry{
			Time: time.Now().UTC(), UID: m.UID, Subject: m.Subject, From: m.From,
			Action: action, FromMailbox: c.cfg.IMAP.Inbox, ToMailbox: created,
			Category: d.Category, Tags: d.Tags, Reasoning: d.Reasoning,
			Confidence: d.Confidence,
		})
		c.state.MarkProcessed(m.UID)
		ui.Decision(m.Subject, m.From, "file", created, d.Reasoning, false)

	case "mark_spam":
		if c.dry {
			ui.Decision(m.Subject, m.From, "spam", c.spamFolder, d.Reasoning, true)
			return
		}
		created, err := c.imap.EnsureMailbox(c.spamFolder)
		if err != nil {
			ui.Warn("uid %d could not ensure spam folder: %v", m.UID, err)
			return
		}
		if err := c.imap.Move(uids, created); err != nil {
			ui.Warn("uid %d spam move failed: %v", m.UID, err)
			return
		}
		c.audit.Append(AuditEntry{
			Time: time.Now().UTC(), UID: m.UID, Subject: m.Subject, From: m.From,
			Action: action, FromMailbox: c.cfg.IMAP.Inbox, ToMailbox: created,
			Reasoning: d.Reasoning, Confidence: d.Confidence,
		})
		c.state.MarkProcessed(m.UID)
		ui.Decision(m.Subject, m.From, "spam", created, d.Reasoning, false)

	case "keep_in_inbox":
		if c.dry {
			ui.Decision(m.Subject, m.From, "keep", "INBOX", d.Reasoning, true)
			return
		}
		if err := c.applyTagsAndFlags(uids, d); err != nil {
			ui.Warn("uid %d flag error: %v", m.UID, err)
		}
		c.state.MarkProcessed(m.UID)
		c.audit.Append(AuditEntry{
			Time: time.Now().UTC(), UID: m.UID, Subject: m.Subject, From: m.From,
			Action: action, FromMailbox: c.cfg.IMAP.Inbox, ToMailbox: c.cfg.IMAP.Inbox,
			Tags: d.Tags, Reasoning: d.Reasoning,
		})
		ui.Decision(m.Subject, m.From, "keep", "INBOX", d.Reasoning, false)
	}
}

func (c *Classifier) applyTagsAndFlags(uids []uint32, d *Decision) error {
	var flags []imap.Flag
	for _, t := range d.Tags {
		t = sanitizeTag(t)
		if t == "" {
			continue
		}
		flags = append(flags, imap.Flag(t))
	}
	if d.Star {
		flags = append(flags, imap.FlagFlagged)
	}
	if len(flags) == 0 {
		return nil
	}
	return c.imap.AddFlags(uids, flags...)
}

var tagRe = regexp.MustCompile(`[^a-z0-9\-_]+`)

func sanitizeTag(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	t = tagRe.ReplaceAllString(t, "-")
	t = strings.Trim(t, "-")
	if t == "" {
		return ""
	}
	// IMAP keyword convention: no spaces, no specials. Prefix for discoverability.
	return "sm-" + t
}

// sanitizeFolder builds a provider-appropriate folder path.
func (c *Classifier) sanitizeFolder(category, sub string) string {
	category = strings.TrimSpace(category)
	sub = strings.TrimSpace(sub)
	if category == "" {
		category = "Uncategorized"
	}
	parts := []string{category}
	if sub != "" {
		parts = append(parts, sub)
	}
	joined := path.Join(parts...)
	if root := strings.TrimSpace(c.cfg.IMAP.ArchiveRoot); root != "" {
		joined = path.Join(root, joined)
	}
	return joined
}

// ProcessStats is returned from Process and merged across all messages.
type ProcessStats struct {
	Total     int
	Filed     int
	Spammed   int
	Kept      int
	Errors    int
	Cancelled int
}

func (p *ProcessStats) merge(o ProcessStats) {
	p.Total += o.Total
	p.Filed += o.Filed
	p.Spammed += o.Spammed
	p.Kept += o.Kept
	p.Errors += o.Errors
	p.Cancelled += o.Cancelled
}

func tallyDecision(s ProcessStats, d *Decision) ProcessStats {
	switch d.Tool {
	case "file_email":
		s.Filed++
	case "mark_spam":
		s.Spammed++
	case "keep_in_inbox":
		s.Kept++
	}
	return s
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
