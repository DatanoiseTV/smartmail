package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// common flag bundle shared by run/watch
type commonFlags struct {
	configPath string
	dryRun     bool
	limit      int
	sinceDays  int
	verbose    bool
}

func registerCommon(fs *flag.FlagSet, f *commonFlags) {
	fs.StringVar(&f.configPath, "config", "", "config file path")
	fs.BoolVar(&f.dryRun, "dry-run", false, "do not modify mailboxes, just show decisions")
	fs.IntVar(&f.limit, "limit", 0, "max messages to process this run (0 = all)")
	fs.IntVar(&f.sinceDays, "since", 0, "only consider messages newer than N days (0 = all unseen)")
	fs.BoolVar(&f.verbose, "verbose", false, "verbose decision reasoning")
}

func cmdRun(ctx context.Context, args []string) error {
	var f commonFlags
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	registerCommon(fs, &f)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	return runOnce(ctx, cfg, f)
}

func runOnce(ctx context.Context, cfg *Config, f commonFlags) error {
	ui.Banner()
	ui.Info("Connecting to %s:%d as %s", cfg.IMAP.Host, cfg.IMAP.Port, cfg.IMAP.Username)
	cli, err := NewClient(cfg.IMAP)
	if err != nil {
		return err
	}
	defer cli.Close()

	if _, err := cli.Select(cfg.IMAP.Inbox); err != nil {
		return fmt.Errorf("select %s: %w", cfg.IMAP.Inbox, err)
	}

	state, err := LoadState(cfg.Paths.StateFile)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	defer state.Save()

	audit, err := OpenAudit(cfg.Paths.AuditFile)
	if err != nil {
		return fmt.Errorf("open audit: %w", err)
	}
	defer audit.Close()

	llm := NewLLM(cfg.LLM)
	cl := NewClassifier(cfg, cli, llm, state, audit, f.dryRun, f.verbose)
	if err := cl.Prepare(); err != nil {
		return err
	}

	ui.Info("Searching unseen messages (since=%dd)", f.sinceDays)
	uids, err := cli.SearchUnseen(f.sinceDays)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	uids = state.FilterUnprocessed(uids)
	if f.limit > 0 && len(uids) > f.limit {
		uids = uids[:f.limit]
	}
	if len(uids) == 0 {
		ui.OK("Nothing to do — inbox is clean.")
		return nil
	}
	ui.Info("Fetching %d message(s)...", len(uids))
	msgs, err := cli.FetchMessages(uids, cfg.Policy.MaxBodyChars)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	ui.Section("Classifying")
	t0 := time.Now()
	stats := cl.Process(ctx, msgs)

	ui.Section("Summary")
	fmt.Printf("  total:     %d\n", stats.Total)
	fmt.Printf("  filed:     %d\n", stats.Filed)
	fmt.Printf("  spam:      %d\n", stats.Spammed)
	fmt.Printf("  kept:      %d\n", stats.Kept)
	fmt.Printf("  errors:    %d\n", stats.Errors)
	fmt.Printf("  elapsed:   %s\n", since(t0))
	if f.dryRun {
		ui.Warn("dry run — no changes were made")
	}
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	var f commonFlags
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	registerCommon(fs, &f)
	daemon := fs.Bool("d", false, "daemonize — fork into the background and write a pidfile")
	pidfile := fs.String("pidfile", "", "path to pidfile when daemonizing (default: <config-dir>/smartmail.pid)")
	logfile := fs.String("logfile", "", "redirect stdout/stderr here when daemonizing (default: <config-dir>/smartmail.log)")
	interval := fs.Int("interval", 180, "seconds between full rescans (safety net alongside IDLE)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, cfgPath, err := LoadConfig(f.configPath)
	if err != nil {
		return err
	}

	if *daemon {
		return daemonize(cfgPath, *pidfile, *logfile, args)
	}

	return watchLoop(ctx, cfg, f, *interval)
}

func watchLoop(ctx context.Context, cfg *Config, f commonFlags, intervalSec int) error {
	ui.Banner()
	ui.Info("Watch mode — Ctrl+C to stop")

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := runOnce(ctx, cfg, f); err != nil {
			ui.Warn("pass failed: %v", err)
			// backoff briefly, then retry — transient IMAP hiccups are common.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(30 * time.Second):
			}
			continue
		}

		// Try to IDLE for up to interval; fall back to sleep if unsupported.
		if err := idleWithFallback(ctx, cfg, intervalSec); err != nil {
			ui.Warn("idle: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Duration(intervalSec) * time.Second):
			}
		}
	}
}

func idleWithFallback(ctx context.Context, cfg *Config, intervalSec int) error {
	cli, err := NewClient(cfg.IMAP)
	if err != nil {
		return err
	}
	defer cli.Close()
	if _, err := cli.Select(cfg.IMAP.Inbox); err != nil {
		return err
	}
	idleCtx, cancel := context.WithTimeout(ctx, time.Duration(intervalSec)*time.Second)
	defer cancel()
	err = cli.Idle(idleCtx)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not supported") {
		<-idleCtx.Done()
		return nil
	}
	return nil
}

// daemonize re-execs the current binary with the same watch arguments minus
// the -d flag, detached from the terminal. Stdout/stderr go to logfile.
func daemonize(cfgPath, pidfile, logfile string, origArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cfgDir := "."
	if cfgPath != "" {
		cfgDir = filepath.Dir(cfgPath)
	}
	if pidfile == "" {
		pidfile = filepath.Join(cfgDir, "smartmail.pid")
	}
	if logfile == "" {
		logfile = filepath.Join(cfgDir, "smartmail.log")
	}
	// Refuse to start if an existing pidfile points to a live process.
	if b, err := os.ReadFile(pidfile); err == nil {
		pid := strings.TrimSpace(string(b))
		if pid != "" {
			if err := exec.Command("kill", "-0", pid).Run(); err == nil {
				return fmt.Errorf("already running (pid %s); remove %s if that's wrong", pid, pidfile)
			}
		}
	}

	// Strip -d and forward the rest.
	childArgs := []string{"watch"}
	for i := 0; i < len(origArgs); i++ {
		a := origArgs[i]
		if a == "-d" || a == "--d" {
			continue
		}
		childArgs = append(childArgs, a)
	}

	lf, err := os.OpenFile(logfile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open logfile: %w", err)
	}
	cmd := exec.Command(exe, childArgs...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Stdin = nil
	// setsid on unix — use SysProcAttr set in a separate file via build tags
	// would be cleaner; this is good enough for mac/linux and avoids cgo.
	cmd.Env = append(os.Environ(), "SMARTMAIL_DAEMON=1")
	if err := cmd.Start(); err != nil {
		return err
	}
	pidStr := fmt.Sprintf("%d\n", cmd.Process.Pid)
	if err := os.WriteFile(pidfile, []byte(pidStr), 0o600); err != nil {
		ui.Warn("write pidfile: %v", err)
	}
	ui.OK("Started smartmail daemon pid=%d", cmd.Process.Pid)
	ui.Dim("logs:    %s", logfile)
	ui.Dim("pidfile: %s", pidfile)
	ui.Dim("stop with: kill $(cat %s)", pidfile)
	// Release the child so it outlives us.
	_ = cmd.Process.Release()
	return nil
}

func cmdFolders(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("folders", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	cli, err := NewClient(cfg.IMAP)
	if err != nil {
		return err
	}
	defer cli.Close()
	folders, err := cli.ListMailboxes()
	if err != nil {
		return err
	}
	ui.Section(fmt.Sprintf("Mailboxes (%d)", len(folders)))
	for _, f := range folders {
		fmt.Println(" ", f)
	}
	return nil
}

func cmdUndo(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("undo", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path")
	last := fs.Int("last", 1, "how many of the most recent actions to undo")
	dry := fs.Bool("dry-run", false, "show what would be undone without touching the server")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	entries, err := ReadAuditTail(cfg.Paths.AuditFile, *last)
	if err != nil {
		return fmt.Errorf("read audit: %w", err)
	}
	if len(entries) == 0 {
		ui.Info("nothing to undo")
		return nil
	}
	cli, err := NewClient(cfg.IMAP)
	if err != nil {
		return err
	}
	defer cli.Close()

	ui.Section(fmt.Sprintf("Undoing %d action(s)", len(entries)))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.FromMailbox == e.ToMailbox {
			ui.Dim("skip uid=%d (no move): %s", e.UID, truncate(e.Subject, 60))
			continue
		}
		fmt.Printf("  uid=%d  %s → %s   %q\n", e.UID, e.ToMailbox, e.FromMailbox, truncate(e.Subject, 60))
		if *dry {
			continue
		}
		if _, err := cli.Select(e.ToMailbox); err != nil {
			ui.Warn("select %s: %v", e.ToMailbox, err)
			continue
		}
		if err := cli.Move([]uint32{e.UID}, e.FromMailbox); err != nil {
			ui.Warn("move back failed: %v", err)
		}
	}
	ui.OK("undo complete")
	return nil
}

func cmdStats(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path")
	n := fs.Int("n", 0, "show only the last N entries (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	entries, err := ReadAuditTail(cfg.Paths.AuditFile, *n)
	if err != nil {
		return err
	}

	byAction := map[string]int{}
	byCategory := map[string]int{}
	for _, e := range entries {
		byAction[e.Action]++
		if e.Category != "" {
			byCategory[e.Category]++
		}
	}
	ui.Section(fmt.Sprintf("Audit summary (%d entries)", len(entries)))
	fmt.Println("By action:")
	for k, v := range byAction {
		fmt.Printf("  %-15s %d\n", k, v)
	}
	if len(byCategory) > 0 {
		fmt.Println("By category:")
		for k, v := range byCategory {
			fmt.Printf("  %-20s %d\n", k, v)
		}
	}
	return nil
}
