package tui

import (
	"math"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/harmonica"
)

// spring is one animated number that glides to its target with a Harmonica
// damped spring instead of jumping.
type spring struct {
	pos, vel, target float64
	s                harmonica.Spring
}

func newSpring(freq, damping, start float64) spring {
	return spring{pos: start, target: start, s: harmonica.NewSpring(harmonica.FPS(60), freq, damping)}
}

func (a *spring) set(target float64) { a.target = target }

// jump moves to v at once, without animating.
func (a *spring) jump(v float64) { a.pos, a.target, a.vel = v, v, 0 }

func (a spring) settled() bool {
	return math.Abs(a.pos-a.target) < 0.01 && math.Abs(a.vel) < 0.02
}

func (a *spring) step() {
	if a.settled() {
		a.pos, a.vel = a.target, 0
		return
	}
	a.pos, a.vel = a.s.Update(a.pos, a.vel, a.target)
	if a.settled() {
		a.pos, a.vel = a.target, 0
	}
}

type frameMsg time.Time

func frame() tea.Cmd {
	return tea.Tick(time.Second/60, func(t time.Time) tea.Msg { return frameMsg(t) })
}

// The springs: the tab pill slides, the list glides when it scrolls, and a
// popup unfolds. Critically damped (1.0) means no overshoot where that would
// look wrong (text cannot be drawn half a cell off).
func newAnimations() (pill, scroll, open spring) {
	return newSpring(16, 1.0, 0), newSpring(18, 1.0, 0), newSpring(14, 0.85, 1)
}
