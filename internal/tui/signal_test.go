package tui

import (
	"context"
	"io"
	"math/rand"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dip-bash/suchi/internal/preview"
	"github.com/dip-bash/suchi/internal/store"
)

func startProgram(t *testing.T, st *store.Store) (*tea.Program, *model, chan os.Signal, func(), chan error) {
	t.Helper()
	m := newModel(context.Background(), st, Options{Protocol: preview.Blocks, Out: io.Discard})
	p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(io.Discard),
		tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler())
	sigc := make(chan os.Signal, 4)
	stop := bridgeSignals(context.Background(), p.Quit, sigc, &m.execing)
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	return p, m, sigc, stop, done
}

func waitDone(t *testing.T, done chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: Run returned %v", what, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: the program did not shut down (deadlock)", what)
	}
}

// Regression: opening suchi windows in quick succession made each new one send
// SIGTERM to the previous one, which sometimes hung forever inside Bubble Tea's
// shutdown, leaving its window open. Fire signals at random moments around
// start-up and shutdown, with and without a competing normal quit.
func TestSignalsNeverDeadlockTheProgram(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/suchi.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, _ = st.AddClip(context.Background(), "some entry", 10, 1<<20)

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 400; i++ {
		p, _, sigc, stop, done := startProgram(t, st)
		sig := []os.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT}[i%3]
		time.Sleep(time.Duration(rng.Intn(4000)) * time.Microsecond) // before, during or after start-up
		sigc <- sig
		if i%2 == 0 { // a user quit arriving at the same moment
			go p.Send(tea.KeyMsg{Type: tea.KeyEsc})
		}
		if i%5 == 0 { // and a second signal, like the next window re-sending SIGTERM
			sigc <- syscall.SIGTERM
		}
		waitDone(t, done, "iteration "+string(rune('0'+i%10)))
		stop()
	}
}

func TestSignalBeforeStartIsNotLost(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/suchi.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 50; i++ {
		m := newModel(context.Background(), st, Options{Protocol: preview.Blocks, Out: io.Discard})
		p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithAltScreen(), tea.WithoutSignalHandler())
		sigc := make(chan os.Signal, 4)
		sigc <- syscall.SIGTERM // delivered before the program exists at all
		stop := bridgeSignals(context.Background(), p.Quit, sigc, &m.execing)
		done := make(chan error, 1)
		time.Sleep(2 * time.Millisecond)
		go func() { _, err := p.Run(); done <- err }()
		waitDone(t, done, "early signal")
		stop()
	}
}

func TestCancelledContextClosesTheProgram(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/suchi.db")
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	m := newModel(ctx, st, Options{Protocol: preview.Blocks, Out: io.Discard})
	p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutSignalHandler())
	stop := bridgeSignals(ctx, p.Quit, make(chan os.Signal), &m.execing)
	defer stop()
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	waitDone(t, done, "context cancel")
}

// While a command snippet runs, Ctrl+C is the command's; SIGTERM still closes suchi afterwards.
func TestSIGINTIsIgnoredWhileACommandRuns(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/suchi.db")
	defer st.Close()
	_, m, sigc, stop, done := startProgram(t, st)
	defer stop()
	m.execing.Store(true)
	sigc <- syscall.SIGINT
	select {
	case <-done:
		t.Fatal("SIGINT must not close suchi while a command is running")
	case <-time.After(300 * time.Millisecond):
	}
	sigc <- syscall.SIGTERM
	waitDone(t, done, "SIGTERM during a command")

	// and without a command, SIGINT does close it
	_, _, sigc2, stop2, done2 := startProgram(t, st)
	defer stop2()
	sigc2 <- syscall.SIGINT
	waitDone(t, done2, "plain SIGINT")
}

func TestQuitAfterExitIsHarmless(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/suchi.db")
	defer st.Close()
	p, _, _, stop, done := startProgram(t, st)
	p.Send(tea.KeyMsg{Type: tea.KeyEsc})
	waitDone(t, done, "normal quit")
	var ran atomic.Bool
	go func() { p.Quit(); ran.Store(true) }() // what a late signal does
	time.Sleep(100 * time.Millisecond)
	if !ran.Load() {
		t.Fatal("Quit on a finished program must return immediately, not block")
	}
	stop()
}
