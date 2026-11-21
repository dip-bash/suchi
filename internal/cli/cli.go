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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dip-bash/suchi/internal/clipboard"
	"github.com/dip-bash/suchi/internal/config"
	"github.com/dip-bash/suchi/internal/daemon"
	"github.com/dip-bash/suchi/internal/fileop"
	"github.com/dip-bash/suchi/internal/pidlock"
	"github.com/dip-bash/suchi/internal/store"
	"github.com/dip-bash/suchi/internal/tui"
	"github.com/dip-bash/suchi/internal/version"
)

const usage = `suchi - clipboard history and snippets for the terminal

Usage:
  suchi [flags]         open the interface (Tab switches Clipboard / Snippets)
  suchi -s              open the interface on the Snippets tab
  suchi -c PATH...      copy files/folders to the clipboard and history
  suchi -x PATH...      cut files/folders to the clipboard and history
  suchi -p              paste the clipboard's files into the current directory
  suchi daemon          start the background clipboard watcher
  suchi stop            stop the watcher
  suchi status          show the watcher, backend and database
  suchi add [TEXT]      add TEXT (or stdin) to the history
  suchi get NAME        print the text of a snippet
  suchi import [FILE]   import history.json from the old Python version
  suchi clear [--all]   delete the history (pinned entries stay unless --all)
  suchi version

Flags (all commands):
  --db PATH         database file   (default: %s)
  --limit N         history size    (default: %d)
  --max-size BYTES  skip bigger clipboard text (default: %d)
  --max-media BYTES skip bigger images (default: 16 MiB)
  --backend         auto | wayland | x11 | tty

Interface flags:
  -s, --selection   open the Snippets tab
  --allow-multiple  do not close an interface that is already open

File clipboard:
  -c, --copy PATH... copy files/folders
  -x, --cut PATH...  cut files/folders
  -p, --paste        paste files/folders into the current directory
                     (text is printed; image bytes go to stdout when redirected)

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
		err = e.runUI(cfg, rest, false)
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
	case "import":
		err = e.runImport(cfg, rest)
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

func (e env) runUI(cfg config.Config, args []string, print bool) error {
	fs := e.flags("", &cfg)
	var multiple, copyPaths, cutPaths, paste, snip bool
	fs.BoolVar(&multiple, "allow-multiple", false, "do not close an interface that is already open")
	fs.BoolVar(&copyPaths, "c", false, "copy files/folders to the clipboard")
	fs.BoolVar(&copyPaths, "copy", false, "copy files/folders to the clipboard")
	fs.BoolVar(&cutPaths, "x", false, "cut files/folders to the clipboard")
	fs.BoolVar(&cutPaths, "cut", false, "cut files/folders to the clipboard")
	fs.BoolVar(&paste, "p", false, "paste files/folders from the clipboard")
	fs.BoolVar(&paste, "paste", false, "paste files/folders from the clipboard")
	fs.BoolVar(&snip, "s", false, "open the Snippets tab")
	fs.BoolVar(&snip, "selection", false, "open the Snippets tab")
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}

	switch {
	case copyPaths && cutPaths:
		return errors.New("choose either -c/--copy or -x/--cut")
	case copyPaths:
		if paste || snip {
			return errors.New("cannot combine -c/--copy with -p or -s")
		}
		return e.runFileAction(cfg, fs.Args(), false)
	case cutPaths:
		if paste || snip {
			return errors.New("cannot combine -x/--cut with -p or -s")
		}
		return e.runFileAction(cfg, fs.Args(), true)
	case paste:
		if snip {
			return errors.New("cannot combine -p/--paste with -s")
		}
		return e.runPaste(cfg, fs.Args())
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	ctx, stop := signalContext()
	defer stop()

	if !multiple {
		// Newest window wins: ask the interface that is already open to close.
		lock, err := pidlock.Takeover(ctx, cfg.UIPIDPath(), 3*time.Second)
		if err != nil {
			return err
		}
		defer lock.Release()
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	res, err := tui.Run(ctx, st, tui.Options{Snips: snip})
	_ = st.Close()
	if err != nil || !res.Chosen {
		return err
	}
	if print {
		return e.printItem(toItem(res))
	}
	b, problem, err := clipboard.Detect(cfg.Backend)
	if err != nil {
		return err
	}
	return clipboard.Deliver(ctx, b, problem, toItem(res), clipboard.Clipboard, e.stdout, e.stderr)
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

// runFileAction records files/folders in the history and puts them on the
// clipboard, marked as a copy or a cut.
func (e env) runFileAction(cfg config.Config, args []string, cut bool) error {
	if len(args) == 0 {
		return errors.New("usage: suchi -c|-x PATH...")
	}
	paths := make([]string, 0, len(args))
	seen := make(map[string]bool, len(args))
	for _, p := range args {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if strings.ContainsAny(abs, "\n\r") {
			return fmt.Errorf("path contains a newline: %q", p)
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		if _, err := os.Lstat(abs); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		paths = append(paths, abs)
	}
	text := strings.Join(paths, "\n")
	ctx, stop := signalContext()
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	added, err := st.AddItem(ctx, store.NewClip{Kind: store.ClipFile, Text: text, Mime: "text/uri-list"},
		store.Limits{Count: cfg.Limit, MaxText: cfg.MaxBytes})
	if err != nil {
		return err
	}
	if !added {
		return errors.New("nothing added (the path list is too large)")
	}

	it := clipboard.Item{Kind: clipboard.KindFiles, Text: text, MIME: "text/uri-list", Cut: cut}
	b, problem, err := clipboard.Detect(cfg.Backend)
	if err != nil {
		return fmt.Errorf("saved to history, but could not set the clipboard: %w", err)
	}
	if err := b.Set(ctx, it, clipboard.Clipboard); err != nil {
		if problem != "" {
			fmt.Fprintf(e.stderr, "suchi: %s\n", problem)
		}
		fmt.Fprintf(e.stderr, "suchi: saved to history, but could not set the clipboard: %v\n", err)
	}
	verb := "copied"
	if cut {
		verb = "cut"
	}
	fmt.Fprintf(e.stdout, "%s %d item(s)\n", verb, len(paths))
	return nil
}

// runPaste puts the clipboard's files into the current directory. Text is
// printed and image bytes go to stdout, so `suchi -p` remains useful outside
// a graphical session.
func (e env) runPaste(cfg config.Config, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: suchi -p")
	}
	ctx, stop := signalContext()
	defer stop()

	b, problem, err := clipboard.Detect(cfg.Backend)
	if err != nil {
		return err
	}
	it, ok, err := b.Read(ctx, clipboard.Clipboard, clipboard.Limits{MaxText: cfg.MaxBytes, MaxMedia: cfg.MaxMedia})
	if err != nil {
		if errors.Is(err, clipboard.ErrNoClipboard) {
			if e.hasTerminal() {
				return e.runUI(cfg, nil, true) // a TTY has no clipboard: pick and print
			}
			return errors.New("no clipboard available here; nothing to paste")
		}
		return err
	}
	if !ok {
		if e.hasTerminal() {
			return e.runUI(cfg, nil, true)
		}
		if problem != "" {
			return fmt.Errorf("%s: nothing to paste", problem)
		}
		return errors.New("nothing to paste")
	}
	if it.Kind != clipboard.KindFiles {
		return e.printItem(it)
	}
	dests, err := fileop.Paste(it.Paths(), ".", it.Cut)
	for _, dst := range dests {
		fmt.Fprintln(e.stdout, dst)
	}
	return err
}

// printItem writes text or image bytes to stdout. Image bytes are refused on
// a terminal so they can never corrupt the screen.
func (e env) printItem(it clipboard.Item) error {
	if it.Kind == clipboard.KindImage {
		if f, ok := e.stdout.(*os.File); ok && isChar(f) {
			return errors.New("refusing to print image bytes to a terminal; redirect it: suchi -p > image.png")
		}
		_, err := e.stdout.Write(it.Data)
		return err
	}
	_, err := io.WriteString(e.stdout, it.Text)
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
		fmt.Fprintf(e.stdout, "suchi daemon started (pid %d, %s backend)\n", pid, b.Name())
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

func (e env) runImport(cfg config.Config, args []string) error {
	fs := e.flags("import", &cfg)
	if err := e.parse(fs, &cfg, args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		cache := os.Getenv("XDG_CACHE_HOME")
		if cache == "" {
			h, _ := os.UserHomeDir()
			cache = filepath.Join(h, ".cache")
		}
		path = filepath.Join(cache, "suchi", "history.json") // the Python version's default
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.ImportLegacyJSON(context.Background(), path, cfg.Limit, cfg.MaxBytes)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "imported %d entries from %s\n", n, path)
	return nil
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

func (e env) hasTerminal() bool {
	f, ok := e.stdin.(*os.File)
	return ok && isChar(f)
}

func isChar(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
