package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// webui serves a small single-page dashboard to inspect state, browse the
// audit log, edit the config, and trigger ad-hoc runs. Single-user,
// HTTP-basic-auth gated by a bcrypt hash of a configured password.

func cmdWebUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("webui", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path")
	addr := fs.String("addr", "127.0.0.1:8787", "address to listen on")
	user := fs.String("user", "", "HTTP basic auth username (default: reads SMARTMAIL_WEB_USER or 'admin')")
	pass := fs.String("pass", "", "HTTP basic auth password (default: reads SMARTMAIL_WEB_PASS; required unless set via env)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, cfgPath, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}

	u := *user
	if u == "" {
		u = firstNonEmpty(os.Getenv("SMARTMAIL_WEB_USER"), "admin")
	}
	p := *pass
	if p == "" {
		p = os.Getenv("SMARTMAIL_WEB_PASS")
	}
	if p == "" {
		return fmt.Errorf("set a password via --pass or SMARTMAIL_WEB_PASS — refusing to serve an unauthenticated UI")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	srv := &webServer{cfg: cfg, cfgPath: cfgPath, user: u, passHash: hash}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.auth(srv.index))
	mux.HandleFunc("/api/status", srv.auth(srv.apiStatus))
	mux.HandleFunc("/api/audit", srv.auth(srv.apiAudit))
	mux.HandleFunc("/api/folders", srv.auth(srv.apiFolders))
	mux.HandleFunc("/api/config", srv.auth(srv.apiConfig))
	mux.HandleFunc("/api/run", srv.auth(srv.apiRun))

	ui.Banner()
	ui.OK("Web UI listening on http://%s (user: %s)", *addr, u)

	s := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdown)
	}()
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

type webServer struct {
	cfg      *Config
	cfgPath  string
	user     string
	passHash []byte

	mu       sync.Mutex
	running  bool
	lastRun  time.Time
	lastStat ProcessStats
}

func (w *webServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userMatch := ok && subtle.ConstantTimeCompare([]byte(u), []byte(w.user)) == 1
		passMatch := userMatch && bcrypt.CompareHashAndPassword(w.passHash, []byte(p)) == nil
		if !passMatch {
			rw.Header().Set("WWW-Authenticate", `Basic realm="smartmail"`)
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(rw, r)
	}
}

func (w *webServer) index(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = rw.Write([]byte(indexHTML))
}

type statusResp struct {
	Version   string       `json:"version"`
	Host      string       `json:"host"`
	Inbox     string       `json:"inbox"`
	Model     string       `json:"model"`
	Provider  string       `json:"provider"`
	Running   bool         `json:"running"`
	LastRun   string       `json:"last_run"`
	LastStats ProcessStats `json:"last_stats"`
}

func (w *webServer) apiStatus(rw http.ResponseWriter, _ *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	writeJSON(rw, statusResp{
		Version: version, Host: w.cfg.IMAP.Host, Inbox: w.cfg.IMAP.Inbox,
		Model: w.cfg.LLM.Model, Provider: w.cfg.LLM.Provider,
		Running: w.running,
		LastRun: func() string {
			if w.lastRun.IsZero() {
				return ""
			}
			return w.lastRun.Format(time.RFC3339)
		}(),
		LastStats: w.lastStat,
	})
}

func (w *webServer) apiAudit(rw http.ResponseWriter, r *http.Request) {
	n := 100
	if s := r.URL.Query().Get("n"); s != "" {
		fmt.Sscanf(s, "%d", &n)
	}
	entries, err := ReadAuditTail(w.cfg.Paths.AuditFile, n)
	if err != nil {
		http.Error(rw, err.Error(), 500)
		return
	}
	writeJSON(rw, entries)
}

func (w *webServer) apiFolders(rw http.ResponseWriter, _ *http.Request) {
	cli, err := NewClient(w.cfg.IMAP)
	if err != nil {
		http.Error(rw, err.Error(), 500)
		return
	}
	defer cli.Close()
	folders, err := cli.ListMailboxes()
	if err != nil {
		http.Error(rw, err.Error(), 500)
		return
	}
	writeJSON(rw, folders)
}

func (w *webServer) apiConfig(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Redact secrets before returning.
		copy := *w.cfg
		if !strings.HasPrefix(copy.IMAP.Password, "env:") {
			copy.IMAP.Password = "***"
		}
		if !strings.HasPrefix(copy.LLM.APIKey, "env:") {
			copy.LLM.APIKey = "***"
		}
		writeJSON(rw, copy)
	case http.MethodPost:
		var newCfg Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		// Preserve secrets if the UI sent the redacted sentinel.
		if newCfg.IMAP.Password == "***" {
			newCfg.IMAP.Password = w.cfg.IMAP.Password
		}
		if newCfg.LLM.APIKey == "***" {
			newCfg.LLM.APIKey = w.cfg.LLM.APIKey
		}
		b, err := yaml.Marshal(&newCfg)
		if err != nil {
			http.Error(rw, err.Error(), 500)
			return
		}
		if err := os.WriteFile(w.cfgPath, b, 0o600); err != nil {
			http.Error(rw, err.Error(), 500)
			return
		}
		// Re-resolve so secrets are re-read from env.
		_ = newCfg.Resolve()
		w.mu.Lock()
		w.cfg = &newCfg
		w.mu.Unlock()
		rw.WriteHeader(204)
	default:
		http.Error(rw, "method not allowed", 405)
	}
}

type runReq struct {
	DryRun    bool `json:"dry_run"`
	Limit     int  `json:"limit"`
	SinceDays int  `json:"since_days"`
}

func (w *webServer) apiRun(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", 405)
		return
	}
	var req runReq
	_ = json.NewDecoder(r.Body).Decode(&req)

	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		http.Error(rw, "already running", 409)
		return
	}
	w.running = true
	cfg := w.cfg
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			w.running = false
			w.lastRun = time.Now()
			w.mu.Unlock()
		}()
		flags := commonFlags{dryRun: req.DryRun, limit: req.Limit, sinceDays: req.SinceDays}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := runOnce(ctx, cfg, flags); err != nil {
			ui.Warn("webui run failed: %v", err)
		}
	}()
	rw.WriteHeader(202)
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Embedded single-page UI. Vanilla HTML/CSS/JS — no build step, no deps.
const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>smartmail</title>
<style>
  :root { color-scheme: dark; --bg:#0b0d10; --panel:#13161b; --ink:#e6e8eb; --dim:#8a93a2; --accent:#7aa2ff; --ok:#67d4a0; --warn:#ffcf6b; --bad:#ff7a7a; }
  * { box-sizing: border-box; }
  body { margin:0; font:14px/1.5 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif; background:var(--bg); color:var(--ink); }
  header { padding:18px 24px; border-bottom:1px solid #1f242c; display:flex; align-items:center; gap:14px; }
  header h1 { font-size:16px; margin:0; letter-spacing:.3px; }
  header .tag { color:var(--dim); font-size:12px; }
  nav { display:flex; gap:6px; padding:8px 18px; border-bottom:1px solid #1f242c; background:var(--panel); }
  nav button { background:none; border:0; color:var(--dim); padding:8px 14px; border-radius:6px; cursor:pointer; font-size:13px; }
  nav button.active { color:var(--ink); background:#1a1f27; }
  main { padding:22px; max-width:1100px; margin:0 auto; }
  .grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(220px,1fr)); gap:12px; margin-bottom:20px; }
  .card { background:var(--panel); padding:14px 16px; border-radius:10px; border:1px solid #1f242c; }
  .card h3 { margin:0 0 6px; font-size:12px; color:var(--dim); font-weight:500; text-transform:uppercase; letter-spacing:.6px; }
  .card .val { font-size:22px; font-weight:600; }
  .row { display:flex; gap:8px; margin:12px 0; flex-wrap:wrap; }
  button.primary { background:var(--accent); color:#0b0d10; border:0; padding:9px 16px; border-radius:7px; font-weight:600; cursor:pointer; }
  button.ghost { background:transparent; color:var(--ink); border:1px solid #2a303a; padding:9px 14px; border-radius:7px; cursor:pointer; }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th, td { padding:9px 10px; text-align:left; border-bottom:1px solid #1a1f27; vertical-align:top; }
  th { color:var(--dim); font-weight:500; font-size:11px; text-transform:uppercase; letter-spacing:.5px; }
  tr:hover td { background:#131821; }
  .pill { display:inline-block; padding:2px 8px; border-radius:999px; font-size:11px; background:#1a1f27; color:var(--dim); }
  .pill.file { background:#1e2c23; color:var(--ok); }
  .pill.spam { background:#2c1e1e; color:var(--bad); }
  .pill.keep { background:#2c291e; color:var(--warn); }
  textarea { width:100%; min-height:460px; background:#0d1117; border:1px solid #1f242c; color:var(--ink); border-radius:8px; padding:12px; font:12px/1.5 ui-monospace,Menlo,monospace; }
  label.chk { display:inline-flex; align-items:center; gap:8px; color:var(--dim); margin-right:14px; }
  input[type=number] { width:90px; }
  input { background:#0d1117; border:1px solid #1f242c; color:var(--ink); border-radius:6px; padding:6px 8px; }
  .muted { color:var(--dim); }
  .hidden { display:none; }
  code { background:#1a1f27; padding:1px 6px; border-radius:4px; font-size:12px; }
</style>
</head>
<body>
<header>
  <h1>smartmail</h1>
  <span class="tag">LLM-powered inbox organizer</span>
  <span id="status-dot" class="tag" style="margin-left:auto"></span>
</header>
<nav>
  <button data-tab="dash" class="active">Dashboard</button>
  <button data-tab="audit">Audit log</button>
  <button data-tab="folders">Folders</button>
  <button data-tab="config">Config</button>
</nav>
<main>
  <section id="dash">
    <div class="grid">
      <div class="card"><h3>Provider</h3><div class="val" id="s-provider">—</div></div>
      <div class="card"><h3>Model</h3><div class="val" id="s-model">—</div></div>
      <div class="card"><h3>IMAP</h3><div class="val" id="s-host">—</div></div>
      <div class="card"><h3>Last run</h3><div class="val" id="s-lastrun">—</div></div>
    </div>
    <div class="card">
      <h3>Trigger a run</h3>
      <div class="row">
        <label class="chk"><input type="checkbox" id="r-dry" checked/> dry run</label>
        <label class="chk">limit <input type="number" id="r-limit" value="25" min="0"/></label>
        <label class="chk">since days <input type="number" id="r-since" value="0" min="0"/></label>
        <button class="primary" id="r-go">Run now</button>
        <span class="muted" id="r-msg"></span>
      </div>
      <div class="muted">Output streams to the server's stdout/log, not the browser.</div>
    </div>
  </section>
  <section id="audit" class="hidden">
    <div class="row">
      <button class="ghost" onclick="loadAudit()">refresh</button>
      <span class="muted">latest 200 actions</span>
    </div>
    <table>
      <thead><tr><th>Time</th><th>Action</th><th>Subject</th><th>From</th><th>From → To</th><th>Why</th></tr></thead>
      <tbody id="audit-rows"></tbody>
    </table>
  </section>
  <section id="folders" class="hidden">
    <div class="row">
      <button class="ghost" onclick="loadFolders()">refresh</button>
    </div>
    <ul id="folder-list" style="columns:2; column-gap:24px; padding-left:18px;"></ul>
  </section>
  <section id="config" class="hidden">
    <div class="row">
      <button class="ghost" onclick="loadConfig()">reload</button>
      <button class="primary" onclick="saveConfig()">save</button>
      <span class="muted" id="cfg-msg"></span>
    </div>
    <textarea id="cfg-text" spellcheck="false"></textarea>
    <div class="muted" style="margin-top:6px">Secrets shown as <code>***</code> are preserved when saving. Use <code>env:NAME</code> to read from environment.</div>
  </section>
</main>
<script>
const $ = s => document.querySelector(s);
document.querySelectorAll('nav button').forEach(b => b.onclick = () => {
  document.querySelectorAll('nav button').forEach(x => x.classList.remove('active'));
  b.classList.add('active');
  ['dash','audit','folders','config'].forEach(id => $('#'+id).classList.add('hidden'));
  $('#'+b.dataset.tab).classList.remove('hidden');
  if (b.dataset.tab === 'audit') loadAudit();
  if (b.dataset.tab === 'folders') loadFolders();
  if (b.dataset.tab === 'config') loadConfig();
});
async function j(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) throw new Error(await r.text());
  const ct = r.headers.get('content-type')||'';
  return ct.includes('json') ? r.json() : r.text();
}
async function refreshStatus() {
  try {
    const s = await j('/api/status');
    $('#s-provider').textContent = s.provider || '—';
    $('#s-model').textContent = s.model || '—';
    $('#s-host').textContent = s.host || '—';
    $('#s-lastrun').textContent = s.last_run ? new Date(s.last_run).toLocaleString() : 'never';
    $('#status-dot').textContent = s.running ? '● running' : '';
  } catch(e) { /* ignore */ }
}
async function loadAudit() {
  const rows = await j('/api/audit?n=200');
  const body = $('#audit-rows');
  body.innerHTML = '';
  rows.reverse().forEach(e => {
    const tr = document.createElement('tr');
    tr.innerHTML =
      '<td>' + new Date(e.time).toLocaleString() + '</td>' +
      '<td><span class="pill ' + pillFor(e.action) + '">' + e.action + '</span></td>' +
      '<td>' + esc(e.subject) + '</td>' +
      '<td>' + esc(e.from) + '</td>' +
      '<td class="muted">' + esc(e.from_mailbox) + ' → ' + esc(e.to_mailbox) + '</td>' +
      '<td class="muted">' + esc(e.reasoning||'') + '</td>';
    body.appendChild(tr);
  });
}
function pillFor(a){ return a==='file_email'?'file':a==='mark_spam'?'spam':'keep'; }
function esc(s){ return (s||'').replace(/[&<>]/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c])); }
async function loadFolders() {
  const list = await j('/api/folders');
  const ul = $('#folder-list');
  ul.innerHTML = '';
  list.forEach(f => { const li = document.createElement('li'); li.textContent = f; ul.appendChild(li); });
}
async function loadConfig() {
  const c = await j('/api/config');
  $('#cfg-text').value = JSON.stringify(c, null, 2);
  $('#cfg-msg').textContent = '';
}
async function saveConfig() {
  $('#cfg-msg').textContent = 'saving…';
  try {
    const body = $('#cfg-text').value;
    JSON.parse(body); // validate
    await j('/api/config', { method:'POST', headers:{'Content-Type':'application/json'}, body });
    $('#cfg-msg').textContent = 'saved';
    setTimeout(() => $('#cfg-msg').textContent = '', 2000);
  } catch(e) { $('#cfg-msg').textContent = 'error: ' + e.message; }
}
$('#r-go').onclick = async () => {
  $('#r-msg').textContent = 'starting…';
  try {
    await j('/api/run', { method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({ dry_run: $('#r-dry').checked, limit: +$('#r-limit').value, since_days: +$('#r-since').value }) });
    $('#r-msg').textContent = 'run queued';
  } catch(e) { $('#r-msg').textContent = 'error: ' + e.message; }
};
refreshStatus();
setInterval(refreshStatus, 5000);
</script>
</body>
</html>`
