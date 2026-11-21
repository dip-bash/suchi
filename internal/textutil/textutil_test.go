package textutil

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDisplayNeutralisesTerminalControls(t *testing.T) {
	in := "snippet\x1b[31mRED\x1b[0m\ttab\r\nnext\u200bline\u0085end"
	got := Display(in)
	for _, r := range got {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("control character %q survived in %q", r, got)
		}
	}
	if want := "snippet [31mRED [0m tab next" + "line end"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDisplayCollapsesAndCaps(t *testing.T) {
	if got := Display("  a \n\n  b\t\tc  "); got != "a b c" {
		t.Fatalf("got %q", got)
	}
	if n := len([]rune(Display(strings.Repeat("é", DisplayCap+100)))); n != DisplayCap {
		t.Fatalf("cap: got %d runes", n)
	}
}

func TestMatchSubstringAndFuzzy(t *testing.T) {
	low := Lower([]rune("héllo wörld mixed"))
	idx, ok := Match(Lower([]rune("WÖR")), low, false)
	if !ok || !reflect.DeepEqual(idx, []int{6, 7, 8}) {
		t.Fatalf("substring: %v %v", idx, ok)
	}
	if _, ok := Match([]rune("zzz"), low, false); ok {
		t.Fatal("unexpected match")
	}
	idx, ok = Match([]rune("hwd"), low, true)
	if !ok || !reflect.DeepEqual(idx, []int{0, 6, 10}) {
		t.Fatalf("fuzzy: %v %v", idx, ok)
	}
	if _, ok := Match([]rune("dw"), low, true); ok {
		t.Fatal("fuzzy must respect order")
	}
	if idx, ok := Match(nil, low, false); !ok || idx != nil {
		t.Fatal("empty query matches everything")
	}
}

func TestLayoutTruncatesByColumnsNotRunes(t *testing.T) {
	// CJK characters are two columns wide.
	segs := Layout([]rune("日本語日本語"), nil, 7, "…")
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}
	if got := b.String(); got != "日本語…" || Width(got) > 7 {
		t.Fatalf("got %q (width %d)", got, Width(got))
	}
	segs = Layout([]rune("abcdef"), []int{1, 2}, 20, "…")
	if len(segs) != 3 || !segs[1].Match || segs[1].Text != "bc" {
		t.Fatalf("match segments: %+v", segs)
	}
}

func TestWrap(t *testing.T) {
	got := Wrap("hello world foo\n\n\tindented\x1b[0m", 11)
	want := []string{"hello world", "foo", "", "    indente", "d·[0m"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRelative(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{
		10 * time.Second: "now", 5 * time.Minute: "5m", 3 * time.Hour: "3h", 50 * time.Hour: "2d",
	} {
		if got := Relative(now.Add(-d), now); got != want {
			t.Errorf("%v: got %q want %q", d, got, want)
		}
	}
	if Relative(time.Time{}, now) != "" {
		t.Error("zero time must be empty")
	}
}
