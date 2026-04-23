package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
)

// Client is a thin wrapper around imapclient that exposes only the operations
// smartmail actually uses. Kept narrow on purpose — easier to reason about
// and easier to mock if we ever add tests.
type Client struct {
	c *imapclient.Client
}

// Message is the decoded view of a mail we send to the LLM.
type Message struct {
	UID         uint32
	Subject     string
	From        string
	To          string
	Cc          string
	Date        time.Time
	ListID      string // List-Id header, if present — strong signal for lists
	Unsubscribe string // List-Unsubscribe
	BodyText    string
	HasHTML     bool
	SizeBytes   uint32
	Flags       []imap.Flag
}

func dialIMAP(addr string, tlsCfg *tls.Config, useTLS bool) (*Client, error) {
	opts := &imapclient.Options{}
	var (
		c   *imapclient.Client
		err error
	)
	if useTLS {
		c, err = imapclient.DialTLS(addr, opts)
	} else {
		c, err = imapclient.DialInsecure(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	_ = tlsCfg
	return &Client{c: c}, nil
}

func NewClient(cfg IMAPConfig) (*Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	var tlsCfg *tls.Config
	if cfg.UseTLS {
		tlsCfg = &tls.Config{ServerName: cfg.Host}
	}
	cli, err := dialIMAP(addr, tlsCfg, cfg.UseTLS)
	if err != nil {
		return nil, err
	}
	if err := cli.Login(cfg.Username, cfg.Password); err != nil {
		cli.Close()
		return nil, err
	}
	return cli, nil
}

func (c *Client) Close() error {
	if c.c == nil {
		return nil
	}
	_ = c.c.Logout().Wait()
	return c.c.Close()
}

func (c *Client) Login(user, pass string) error {
	return c.c.Login(user, pass).Wait()
}

// ListMailboxes returns every mailbox path visible to the account.
func (c *Client) ListMailboxes() ([]string, error) {
	data, err := c.c.List("", "*", nil).Collect()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(data))
	for _, m := range data {
		out = append(out, m.Mailbox)
	}
	return out, nil
}

// Select opens a mailbox read-write.
func (c *Client) Select(mailbox string) (*imap.SelectData, error) {
	return c.c.Select(mailbox, &imap.SelectOptions{ReadOnly: false}).Wait()
}

// SearchUnseen returns UIDs of unseen messages. If sinceDays > 0 the search is
// further restricted to messages internally dated within that window.
func (c *Client) SearchUnseen(sinceDays int) ([]uint32, error) {
	crit := &imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}
	if sinceDays > 0 {
		crit.Since = time.Now().AddDate(0, 0, -sinceDays)
	}
	data, err := c.c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return nil, err
	}
	in := data.AllUIDs()
	out := make([]uint32, len(in))
	for i, u := range in {
		out[i] = uint32(u)
	}
	return out, nil
}

func toUIDs(uids []uint32) []imap.UID {
	out := make([]imap.UID, len(uids))
	for i, u := range uids {
		out[i] = imap.UID(u)
	}
	return out
}

// FetchMessages returns fully-decoded Message values for the given UIDs.
func (c *Client) FetchMessages(uids []uint32, maxBody int) ([]*Message, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	set := imap.UIDSetNum(toUIDs(uids)...)
	opts := &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		InternalDate: true,
		RFC822Size:   true,
		Envelope:     true,
		BodySection:  []*imap.FetchItemBodySection{{}},
	}
	msgs, err := c.c.Fetch(set, opts).Collect()
	if err != nil {
		return nil, err
	}
	out := make([]*Message, 0, len(msgs))
	for _, m := range msgs {
		dm, err := decodeMessage(m, maxBody)
		if err != nil {
			ui.Warn("decode uid %d: %v", m.UID, err)
			continue
		}
		out = append(out, dm)
	}
	return out, nil
}

func decodeMessage(m *imapclient.FetchMessageBuffer, maxBody int) (*Message, error) {
	out := &Message{
		UID:       uint32(m.UID),
		Flags:     m.Flags,
		SizeBytes: uint32(m.RFC822Size),
	}
	if m.Envelope != nil {
		out.Subject = m.Envelope.Subject
		out.Date = m.Envelope.Date
		out.From = addrList(m.Envelope.From)
		out.To = addrList(m.Envelope.To)
		out.Cc = addrList(m.Envelope.Cc)
	}
	// Grab the first body section we got.
	var raw []byte
	for _, v := range m.BodySection {
		raw = v.Bytes
		break
	}
	if len(raw) == 0 {
		return out, nil
	}
	mr, err := mail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		// Fall back to treating as plain text.
		out.BodyText = truncateBody(string(raw), maxBody)
		return out, nil
	}
	out.ListID = mr.Header.Get("List-Id")
	out.Unsubscribe = mr.Header.Get("List-Unsubscribe")
	var textBuf strings.Builder
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			b, _ := io.ReadAll(p.Body)
			if strings.HasPrefix(ct, "text/plain") {
				if textBuf.Len() > 0 {
					textBuf.WriteString("\n")
				}
				textBuf.Write(b)
			} else if strings.HasPrefix(ct, "text/html") {
				out.HasHTML = true
				if textBuf.Len() == 0 {
					textBuf.WriteString(stripHTML(string(b)))
				}
			}
		case *mail.AttachmentHeader:
			// Ignore attachments — we only care about semantics for classification.
			_ = h
		}
		if textBuf.Len() >= maxBody {
			break
		}
	}
	out.BodyText = truncateBody(textBuf.String(), maxBody)
	return out, nil
}

func addrList(in []imap.Address) string {
	parts := make([]string, 0, len(in))
	for _, a := range in {
		name := strings.TrimSpace(a.Name)
		addr := fmt.Sprintf("%s@%s", a.Mailbox, a.Host)
		if name != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", name, addr))
		} else {
			parts = append(parts, addr)
		}
	}
	return strings.Join(parts, ", ")
}

func truncateBody(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", "")
	// Collapse runs of blank lines — most mail has vast whitespace padding.
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "\n…[truncated]"
}

// stripHTML is a deliberately crude HTML-to-text. We only need enough signal
// for the LLM to classify — not perfect rendering.
func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteRune(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// EnsureMailbox creates a mailbox if it doesn't already exist. Returns the
// possibly-adjusted path (provider hierarchy delimiters normalised).
func (c *Client) EnsureMailbox(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("empty mailbox path")
	}
	// If it already exists we're done.
	existing, err := c.c.List("", path, nil).Collect()
	if err == nil && len(existing) > 0 {
		return existing[0].Mailbox, nil
	}
	if err := c.c.Create(path, nil).Wait(); err != nil {
		// Gmail in particular rejects certain names; surface the error.
		return "", fmt.Errorf("create mailbox %q: %w", path, err)
	}
	// Subscribe so the user sees it in their client.
	_ = c.c.Subscribe(path).Wait()
	return path, nil
}

// Move relocates messages to the target mailbox. go-imap v2's Move command
// already falls back to COPY + STORE + EXPUNGE on servers without MOVE.
func (c *Client) Move(uids []uint32, target string) error {
	if len(uids) == 0 {
		return nil
	}
	set := imap.UIDSetNum(toUIDs(uids)...)
	_, err := c.c.Move(set, target).Wait()
	return err
}

// AddFlags applies one or more flags (e.g. \Seen, \Flagged, or a keyword).
func (c *Client) AddFlags(uids []uint32, flags ...imap.Flag) error {
	if len(uids) == 0 || len(flags) == 0 {
		return nil
	}
	set := imap.UIDSetNum(toUIDs(uids)...)
	return c.c.Store(set, &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  flags,
		Silent: true,
	}, nil).Close()
}

// Idle blocks until the server notifies us of inbox changes, or ctx is
// cancelled. Returns nil on normal exit, error on connection issue.
func (c *Client) Idle(ctx context.Context) error {
	idle, err := c.c.Idle()
	if err != nil {
		return fmt.Errorf("start idle: %w", err)
	}
	<-ctx.Done()
	if err := idle.Close(); err != nil {
		return err
	}
	return idle.Wait()
}

// Noop keeps the connection alive. Cheap; call it periodically.
func (c *Client) Noop() error {
	return c.c.Noop().Wait()
}
