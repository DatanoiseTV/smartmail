package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		ui.Warn("\ninterrupt received, shutting down cleanly...")
		cancel()
	}()

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "init":
		err = cmdInit(ctx, args)
	case "run":
		err = cmdRun(ctx, args)
	case "watch":
		err = cmdWatch(ctx, args)
	case "folders":
		err = cmdFolders(ctx, args)
	case "undo":
		err = cmdUndo(ctx, args)
	case "stats":
		err = cmdStats(ctx, args)
	case "webui":
		err = cmdWebUI(ctx, args)
	case "version", "-v", "--version":
		fmt.Println("smartmail", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(2)
	}

	if err != nil {
		ui.Fail(err.Error())
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`smartmail — LLM-powered IMAP inbox organizer

USAGE
  smartmail <command> [flags]

COMMANDS
  init      Interactive setup wizard — creates a config file
  run       Classify and organize new mail (one pass, then exit)
  watch     Stay connected via IMAP IDLE and classify in real time
  folders   List mailboxes on the server
  undo      Roll back a previous action from the audit log
  stats     Show processing statistics
  webui     Serve a login-protected web UI for management
  version   Show version
  help      Show this message

COMMON FLAGS
  --config PATH   Path to config file (default: ./smartmail.yaml or $XDG_CONFIG_HOME/smartmail/config.yaml)
  --dry-run       Do not actually move anything — just show what would happen
  --limit N       Process at most N messages this run
  --since DAYS    Only look at messages newer than N days (default: all unread)
  --verbose       Print full LLM reasoning for each decision

EXAMPLES
  smartmail init
  smartmail run --dry-run --limit 20 --verbose
  smartmail watch
  smartmail undo --last 5
`)
	_ = flag.ErrHelp
}
