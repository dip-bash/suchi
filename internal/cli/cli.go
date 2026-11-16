// Package cli parses the command line and runs suchi's commands.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dip-bash/suchi/internal/clipboard"
	"github.com/dip-bash/suchi/internal/config"
	"github.com/dip-bash/suchi/internal/daemon"
	"github.com/dip-bash/suchi/internal/filebundle"
	"github.com/dip-bash/suchi/internal/pidlock"
	"github.com/dip-bash/suchi/internal/store"
	"github.com/dip-bash/suchi/internal/tui"
	"github.com/dip-bash/suchi/internal/version"
)

const usage = `suchi - clipboard history and snippets for the terminal

Usage:
  suchi [flags]         open the interface (Tab switches Clipboard / Snippets)
  suchi daemon          start the background clipboard watcher
  suchi stop            stop the watcher
  suchi status          show the watcher, backend and database
  suchi add [TEXT]      add TEXT (or stdin) to the history
  suchi get NAME        print the text of a snippet
  suchi clear [--all]   delete the history (pinned entries stay unless --all)
  suchi version

File transfer:
  suchi -c FILE...      copy files/directories to the CLI clipboard
  suchi -x FILE...      cut files/directories (remove after clipboard is set)
  suchi -p [DIR]        paste a file bundle into DIR (default: current directory)

Flags (all commands):
  --db PATH         database file   (default: %s)
  --limit N         history size    (default: %d)
  --max-size BYTES  skip bigger clipboard text (default: %d)
  --max-media BYTES skip bigger images (default: 16 MiB)
  -s, --selection   clipboard | primary
  --backend         auto | wayland | x11 | tty

Interface flags:
  --print           print the chosen text to stdout instead of copying it
  --ascii           plain ASCII glyphs (automatic on the Linux console)
  --allow-multiple  do not close an interface that is already open

Only one suchi interface runs at a time: opening a new one closes the previous
one, so a key binding like "footclient -W 80x30 suchi" never piles up windows.
`

// env carries the process streams so commands are testable.
type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// Run executes suchi with args (without the program name) and returns the exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := env{stdin, stdout, stderr}
	cfg := config.Default()

	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprintf(stdout, usage, cfg.DBPath, cfg.Limit, cfg.MaxBytes)
		return 0
	}
	cmd, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, rest = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "":
		err = e.runUI(cfg, rest)
	case "daemon":
		err = e.runDaemon(cfg, rest)
	case "stop":
		err = e.runStop(cfg, rest)
	case "status":
		err = e.runStatus(cfg, rest)
	case "add":
		err = e.runAdd(cfg, rest)
	case "get":
		err = e.runGet(cfg, rest)
	case "clear":
		err = e.runClear(cfg, rest)
	case "version":
		fmt.Fprintln(stdout, version.String())
	case clipboard.HelperArg:
		err = e.runServe(rest)
	default:
		fmt.Fprintf(stderr, "suchi: unknown command %q\n\n", cmd)
		fmt.Fprintf(stderr, usage, cfg.DBPath, cfg.Limit, cfg.MaxBytes)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, context.Canceled):
		return 130
	}
	fmt.Fprintf(stderr, "suchi: %v\n", err)
	return 1
}

var errUsage = errors.New("usage")

// flags builds a FlagSet with the options every command shares.
func (e env) flags(name string, cfg *config.Config) *flag.FlagSet {
	fs := flag.NewFlagSet("suchi "+name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "database file")
	fs.IntVar(&cfg.Limit, "limit", cfg.Limit, "history size")
	fs.IntVar(&cfg.MaxBytes, "max-size", cfg.MaxBytes, "skip bigger clipboard text")
	fs.IntVar(&cfg.MaxMedia, "max-media", cfg.MaxMedia, "skip bigger images")
	fs.StringVar(&cfg.Selection, "selection", cfg.Selection, "clipboard | primary")
	fs.StringVar(&cfg.Selection, "s", cfg.Selection, "clipboard | primary")
	fs.StringVar(&cfg.Backend, "backend", cfg.Backend, "auto | wayland | x11 | tty")
	return fs
}

func (e env) parse(fs *flag.FlagSet, cfg *config.Config, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	return cfg.Validate()
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}

func (e env) runUI(cfg config.Config, args []string) error {
	fs := e.flags("", &cfg)
	var print, ascii, multiple, copyFiles, cutFiles, pasteFiles bool
	fs.BoolVar(&multiple, "allow-multiple", false, "do not close an interface that is already open")
	fs.BoolVar(&print, "print", false, "print the chosen text")
	fs.BoolVar(&ascii, "ascii", false, "plain ASCII glyphs")
	fs.BoolVar(&copyFiles, "c", false, "copy files/directories to the CLI clipboard")
	fs.BoolVar(&copyFiles, "copy", false, "copy files/directories to the CLI clipboard")
	fs.BoolVar(&cutFiles, "x", false, "cut files/directories to the CLI clipboard")
	fs.BoolVar(&cutFiles, "cut", false, "cut files/directories to the CLI clipboard")
	fs.BoolVar(&pasteFiles, "p", false, "paste a CLI file bundle")
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}
	selectedOps := 0
	for _, selected := range []bool{copyFiles, cutFiles, pasteFiles} {
		if selected {
			selectedOps++
		}
	}
	if selectedOps > 1 || (selectedOps > 0 && print) {
		return fmt.Errorf("choose only one of -c, -x, -p or --print: %w", errUsage)
	}
	if copyFiles || cutFiles {
		if fs.NArg() == 0 {
			return fmt.Errorf("-c and -x need at least one file or directory: %w", errUsage)
		}
		return e.runFileCopy(cfg, fs.Args(), cutFiles)
	}
	if pasteFiles {
		if fs.NArg() > 1 {
			return fmt.Errorf("usage: suchi -p [DIR]: %w", errUsage)
		}
		destination := "."
		if fs.NArg() == 1 {
			destination = fs.Arg(0)
		}
		return e.runFilePaste(cfg, destination)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s: %w", strings.Join(fs.Args(), " "), errUsage)
	}
	ctx, stop := signalContext()
	defer stop()

	var uiLock *pidlock.Lock
	if !multiple {
		// Newest window wins: ask the interface that is already open to close.
		var lockErr error
		uiLock, lockErr = pidlock.Takeover(ctx, cfg.UIPIDPath(), 3*time.Second)
		if lockErr != nil {
			return lockErr
		}
		// Keep a fallback release for every early return, but release it as soon
		// as the TUI and its database are closed (before clipboard delivery).
		defer func() {
			if uiLock != nil {
				uiLock.Release()
			}
		}()
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	res, runErr := tui.Run(ctx, st, tui.Options{ASCII: ascii})
	closeErr := st.Close()
	if uiLock != nil {
		uiLock.Release()
		uiLock = nil
	}
	if runErr != nil {
		return runErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !res.Chosen {
		return nil
	}
	if print {
		return e.printResult(res)
	}
	b, problem, err := clipboard.Detect(cfg.Backend)
	if err != nil {
		return err
	}
	return clipboard.Deliver(ctx, b, problem, toItem(res), clipboard.Selection(cfg.Selection), e.stdout, e.stderr)
}

// toItem converts the interface's result into a clipboard item.
func toItem(r tui.Result) clipboard.Item {
	switch r.Kind {
	case store.ClipImage:
		return clipboard.Item{Kind: clipboard.KindImage, MIME: r.MIME, Data: r.Data}
	case store.ClipFile:
		return clipboard.Item{Kind: clipboard.KindFiles, MIME: r.MIME, Text: r.Text}
	}
	return clipboard.Item{Kind: clipboard.KindText, Text: r.Text}
}

// printResult writes the chosen entry to stdout: text and file paths as
// text, an image as raw bytes (`suchi --print > shot.png`), but never into a terminal.
func (e env) printResult(r tui.Result) error {
	if r.Kind == store.ClipImage {
		if f, ok := e.stdout.(*os.File); ok && isChar(f) {
			return errors.New("refusing to print image bytes to a terminal; redirect it: suchi --print > image.png")
		}
		_, err := e.stdout.Write(r.Data)
		return err
	}
	_, err := io.WriteString(e.stdout, r.Text)
	return err
}

func (e env) runDaemon(cfg config.Config, args []string) error {
	fs := e.flags("daemon", &cfg)
	var foreground bool
	var readyFD int
	fs.BoolVar(&foreground, "foreground", false, "run in this terminal instead of detaching")
	fs.IntVar(&readyFD, "ready-fd", 0, "internal: report start-up on this file descriptor")
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}

	if !foreground {
		b, err := daemon.Backend(cfg)
		if err != nil {
			return err
		}
		pid, err := daemon.Start(cfg)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "suchi daemon started (pid %d, %s backend, watching %s)\n", pid, b.Name(), cfg.Selection)
		return nil
	}

	log := slog.New(slog.NewTextHandler(e.stderr, nil))
	slog.SetDefault(log)
	var ready func(error)
	if readyFD > 0 {
		f := os.NewFile(uintptr(readyFD), "ready")
		ready = func(err error) {
			if err != nil {
				fmt.Fprintf(f, "err: %s\n", strings.ReplaceAll(err.Error(), "\n", "\\n"))
			} else {
				fmt.Fprintln(f, "ok")
			}
			_ = f.Close()
		}
	}
	ctx, stop := signalContext()
	defer stop()
	err := daemon.Foreground(ctx, cfg, ready)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (e env) runStop(cfg config.Config, args []string) error {
	fs := e.flags("stop", &cfg)
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}
	stopped, err := daemon.Stop(cfg.PIDPath())
	if err != nil {
		return err
	}
	if stopped {
		fmt.Fprintln(e.stdout, "suchi daemon stopped")
	} else {
		fmt.Fprintln(e.stdout, "suchi daemon is not running")
	}
	return nil
}

func (e env) runStatus(cfg config.Config, args []string) error {
	fs := e.flags("status", &cfg)
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}
	if running, pid := daemon.Status(cfg.PIDPath()); running {
		fmt.Fprintf(e.stdout, "daemon    running (pid %d)\n", pid)
	} else {
		fmt.Fprintln(e.stdout, "daemon    not running (start it with `suchi daemon`)")
	}
	b, problem, err := clipboard.Detect(cfg.Backend)
	switch {
	case err != nil:
		fmt.Fprintf(e.stdout, "backend   unavailable: %v\n", err)
	case b.Name() == "tty" && problem != "":
		fmt.Fprintf(e.stdout, "backend   tty (graphical session not usable: %s)\n", problem)
	default:
		fmt.Fprintf(e.stdout, "backend   %s\n", b.Name())
	}
	fmt.Fprintf(e.stdout, "database  %s\n", cfg.DBPath)
	if _, statErr := os.Stat(cfg.DBPath); statErr == nil {
		st, err := store.Open(cfg.DBPath)
		if err != nil {
			return err
		}
		defer st.Close()
		clips, _ := st.ListClips(context.Background())
		snips, _ := st.ListSnippets(context.Background())
		fmt.Fprintf(e.stdout, "entries   %d clipboard, %d snippets\n", len(clips), len(snips))
	}
	return nil
}

func (e env) runAdd(cfg config.Config, args []string) error {
	fs := e.flags("add", &cfg)
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}
	var text string
	if fs.NArg() == 0 || (fs.NArg() == 1 && fs.Arg(0) == "-") {
		b, err := io.ReadAll(e.stdin)
		if err != nil {
			return err
		}
		text = string(b)
	} else {
		text = strings.Join(fs.Args(), " ")
	}
	text = strings.TrimSuffix(text, "\n") // `echo foo | suchi add` stores "foo"
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	added, err := st.AddClip(context.Background(), text, cfg.Limit, cfg.MaxBytes)
	if err != nil {
		return err
	}
	if !added {
		return errors.New("nothing added (empty, or larger than --max-size)")
	}
	return nil
}

func (e env) runGet(cfg config.Config, args []string) error {
	fs := e.flags("get", &cfg)
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.stderr, "usage: suchi get NAME")
		return errUsage
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	found, err := st.FindSnippets(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("no snippet named %q", fs.Arg(0))
	}
	if len(found) > 1 {
		fmt.Fprintf(e.stderr, "suchi: %d snippets are named %q; printing the first\n", len(found), fs.Arg(0))
	}
	out := found[0].Body
	if f, ok := e.stdout.(*os.File); ok && isChar(f) {
		out += "\n"
	}
	_, err = io.WriteString(e.stdout, out)
	return err
}

func (e env) runFileCopy(cfg config.Config, paths []string, cut bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if cut {
		if err := filebundle.ValidateRemoveSources(paths); err != nil {
			return err
		}
	}
	archive, err := os.CreateTemp("", "suchi-file-bundle-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temporary archive: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	if err := filebundle.CreateTo(archive, paths); err != nil {
		_ = archive.Close()
		return err
	}
	stat, err := archive.Stat()
	if err != nil {
		_ = archive.Close()
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		_ = archive.Close()
		return err
	}
	b, problem, err := clipboard.Detect(cfg.Backend)
	if err != nil {
		_ = archive.Close()
		return err
	}
	if b.Name() == "tty" {
		_ = archive.Close()
		if problem != "" {
			return fmt.Errorf("no graphical clipboard available (%s)", problem)
		}
		return errors.New("file bundles require an X11 or Wayland clipboard")
	}
	if err := clipboard.SetFileBundleReader(ctx, b, archive); err != nil {
		_ = archive.Close()
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if cut {
		if err := filebundle.RemoveSources(paths); err != nil {
			return fmt.Errorf("file bundle is on the clipboard, but source removal failed: %w", err)
		}
	}
	action := "copied"
	if cut {
		action = "cut"
	}
	fmt.Fprintf(e.stdout, "%s %d path(s) (%s) to the file-transfer clipboard\n", action, len(paths), humanBytes(stat.Size()))
	return nil
}

func (e env) runFilePaste(cfg config.Config, destination string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, problem, err := clipboard.Detect(cfg.Backend)
	if err != nil {
		return err
	}
	if b.Name() == "tty" {
		if problem != "" {
			return fmt.Errorf("no graphical clipboard available (%s)", problem)
		}
		return errors.New("file bundles require an X11 or Wayland clipboard")
	}
	archive, err := os.CreateTemp("", "suchi-file-bundle-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temporary archive: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	if err := clipboard.ReadFileBundleTo(ctx, b, archive); err != nil {
		_ = archive.Close()
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		_ = archive.Close()
		return err
	}
	names, err := filebundle.ExtractReader(archive, destination)
	_ = archive.Close()
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "pasted %d item(s) into %s: %s\n", len(names), destination, strings.Join(names, ", "))
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (e env) runClear(cfg config.Config, args []string) error {
	fs := e.flags("clear", &cfg)
	var all, yes bool
	fs.BoolVar(&all, "all", false, "also delete pinned entries")
	fs.BoolVar(&yes, "y", false, "do not ask for confirmation")
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}
	if !yes {
		what := "all unpinned clipboard history"
		if all {
			what = "ALL clipboard history, including pinned entries"
		}
		fmt.Fprintf(e.stderr, "Delete %s? [y/N] ", what)
		line, _ := bufio.NewReader(e.stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(e.stderr, "cancelled")
			return nil
		}
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.ClearClips(context.Background(), all)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "deleted %d entries\n", n)
	return nil
}

// runServe is the hidden helper that keeps a Wayland selection alive.
func (e env) runServe(args []string) error {
	sel := clipboard.Clipboard
	if len(args) > 0 && args[0] == string(clipboard.Primary) {
		sel = clipboard.Primary
	}
	ctx, stop := signalContext()
	defer stop()
	return clipboard.ServeSelection(ctx, sel, e.stdin, e.stdout)
}

func isChar(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
