# smartmail

LLM-powered IMAP inbox organizer. A single Go binary that connects to any mail
server, classifies incoming messages with an LLM using proper tool-calling,
and organizes them into category folders — without ever deleting anything, so
you always have a safe path back.

Works with Gmail, iCloud, Fastmail, Outlook, Yahoo, Proton Bridge, or any
self-hosted IMAP server. Uses OpenAI or any OpenAI-compatible endpoint
(LMStudio, Ollama with `--openai`, vLLM, llama.cpp server, …) so you can run
fully local if you prefer.

## Highlights

- **Single static binary.** No Docker, no Python, no Node. Just `go build`.
- **Real IMAP, real tool calls.** Uses three well-typed tools — `file_email`,
  `mark_spam`, `keep_in_inbox` — with JSON Schema parameters. Not prompt-hacked
  regex parsing.
- **Non-destructive.** Everything is a move. An append-only audit log records
  every action with reasoning, and `smartmail undo` rolls back the last N.
- **Smart folder layout.** The model sees your existing mailboxes and prefers
  to reuse them instead of creating `Newsletters-1/Newsletters-2/…`.
- **Confidence gate.** Low-confidence decisions fall back to leaving the
  message in the inbox — you set the threshold.
- **IMAP keywords as tags.** Cross-folder semantic search (prefix `sm-`).
- **Pre-filters.** Trusted / blocked sender rules skip the LLM to save tokens.
- **Real-time watch mode.** IMAP IDLE + periodic rescan as a safety net.
- **Daemon mode.** `smartmail watch -d` forks into the background with a
  pidfile and logfile.
- **Web UI.** Login-protected dashboard (HTTP basic auth) to trigger runs,
  browse the audit log, view folders, and edit the config.
- **Privacy.** Nothing leaves your machine if you point it at a local
  LMStudio/Ollama endpoint.

## Install

Requires Go 1.22+.

```sh
git clone https://github.com/DatanoiseTV/smartmail.git
cd smartmail
go build -o smartmail .
# optional:
sudo mv smartmail /usr/local/bin/
```

## Quick start

```sh
smartmail init     # interactive setup wizard; writes smartmail.yaml
smartmail run --dry-run --limit 20 --verbose
smartmail run      # when you're happy with the dry-run output
smartmail watch    # stay connected and process in real time
```

### Gmail

Enable 2FA and create an [App Password](https://myaccount.google.com/apppasswords).
Use `imap.gmail.com:993` with TLS. For the archive root, `[Gmail]/All Mail`
works but top-level labels are usually friendlier — Gmail treats folders as
labels, so whatever you pick will show up in the Gmail web UI.

### Local LLM via LMStudio

1. Launch LMStudio, load a model that supports tool calls (Llama 3.1 8B+,
   Qwen2.5 7B+, Mistral Small 3, etc.), and start the local server.
2. In `smartmail init` pick "LMStudio" and confirm `http://localhost:1234/v1`.
3. That's it — everything works offline from here.

## Commands

```
smartmail init                   Interactive setup wizard
smartmail run [flags]            One pass over unseen mail, then exit
smartmail watch [flags]          Long-running: IMAP IDLE + periodic rescan
smartmail watch -d               Daemonize (background, pidfile, logfile)
smartmail folders                List mailboxes on the server
smartmail undo --last N          Roll back the last N actions
smartmail stats                  Summarize the audit log
smartmail webui --pass XXX       Serve the web UI on 127.0.0.1:8787
```

### Common flags

| flag | meaning |
|------|---------|
| `--config PATH` | override config discovery |
| `--dry-run` | print decisions without touching mailboxes |
| `--limit N` | process at most N messages this run |
| `--since DAYS` | only look at messages newer than N days |
| `--verbose` | show full LLM reasoning |

## How it decides

For each unseen message the classifier:

1. Checks **deterministic rules** first (trusted / blocked sender lists) to
   avoid spending tokens on things with obvious answers.
2. Builds a compact prompt: headers, `List-Id`, `List-Unsubscribe`,
   truncated body (HTML stripped), plus the list of **existing mailboxes** on
   the server so the model can reuse them.
3. Forces the model to call **exactly one** of three tools:
   - `file_email(category, subfolder?, tags[], priority, should_star, confidence, reasoning)`
   - `mark_spam(indicators[], confidence, reasoning)`
   - `keep_in_inbox(priority, should_flag, tags[], reasoning)`
4. If the returned confidence is below the configured threshold, the
   decision is downgraded to `keep_in_inbox`.
5. Applies IMAP operations (create folder if missing, add keywords / `\Flagged`,
   then `MOVE` — with automatic fallback to `COPY + STORE + EXPUNGE` on older
   servers).
6. Appends a JSONL entry to the audit log.

## Configuration

A minimal `smartmail.yaml`:

```yaml
imap:
  host: imap.gmail.com
  port: 993
  username: you@example.com
  password: env:SMARTMAIL_IMAP_PASSWORD
  tls: true
  inbox: INBOX
  archive_root: ""          # leave empty for top-level folders

llm:
  provider: openai          # or "lmstudio"
  base_url: https://api.openai.com/v1
  api_key: env:OPENAI_API_KEY
  model: gpt-4o-mini
  temperature: 0
  max_tokens: 800
  timeout_sec: 60

policy:
  min_confidence: 0.65      # below this, keep in inbox
  max_body_chars: 4000
  mark_seen: false
  concurrency: 3

rules:
  trusted_senders:
    - boss@example.com
    - "@partner-company.com"
  blocked_senders:
    - "@obvious-spammer.biz"
  pinned_folders:
    - Personal
    - Work
    - Finance
    - Receipts
    - Shopping
    - Travel
    - Newsletters
    - Marketing
    - Social
    - Notifications
    - Updates
```

Any secret field can be replaced with `env:VAR_NAME` to read it from the
environment instead of storing it in the file.

## Web UI

```sh
export SMARTMAIL_WEB_PASS='choose-a-strong-one'
smartmail webui --addr 127.0.0.1:8787
```

Then browse to `http://127.0.0.1:8787`, log in (default user: `admin`), and
you get a dashboard with status, audit log, folder list, editable config,
and an on-demand "Run now" button.

Serve it behind a reverse proxy if you want to expose it beyond localhost.

## Daemonizing

```sh
smartmail watch -d \
  --pidfile /var/run/smartmail.pid \
  --logfile /var/log/smartmail.log
```

Default paths are `smartmail.pid` and `smartmail.log` next to your config
file. To stop:

```sh
kill $(cat /var/run/smartmail.pid)
```

systemd unit (example):

```ini
[Unit]
Description=smartmail inbox organizer
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/smartmail watch --config /etc/smartmail/config.yaml
Restart=on-failure
User=smartmail
EnvironmentFile=/etc/smartmail/env

[Install]
WantedBy=default.target
```

## Undo / safety

Every action is a single-line JSON entry in `smartmail.audit.log`. The state
file `smartmail.state.json` prevents reprocessing the same UID twice.

```sh
smartmail undo --last 5           # roll back the most recent five moves
smartmail undo --last 5 --dry-run # preview first
```

## Roadmap

- OAuth2 (XOAUTH2) for Gmail / Outlook — today only basic auth + app
  passwords are supported.
- Learning loop: feed the user's manual moves back as few-shot examples.
- Per-mailbox rules (e.g. different policy for a shared team inbox).
- Encrypted password-at-rest via OS keyring.

## License

MIT.
