package tui

import (
	"strings"
	"testing"
)

func TestDrawSplitGraphHeights(t *testing.T) {
	rx := []float64{10, 20, 30, 40, 50, 40, 30}
	tx := []float64{5, 10, 15, 20, 25, 20, 15}

	for _, h := range []int{2, 4, 6, 8, 10, 15} {
		t.Run(string(rune('0'+h)), func(t *testing.T) {
			b := New(50, h)
			shownRx, shownTx := DrawSplitGraph(b, SplitGraph{
				RxSamples: rx,
				TxSamples: tx,
				Symbols:   GraphBraille,
				Format:    RateShort,
			}, false)
			if shownRx <= 0 || shownTx <= 0 {
				t.Fatalf("height %d: shownRx=%v shownTx=%v want > 0", h, shownRx, shownTx)
			}
			out := b.String()
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if len(lines) != h {
				t.Fatalf("height %d rendered %d lines", h, len(lines))
			}
		})
	}
}

func TestDrawSplitGraphMirroredAxis(t *testing.T) {
	rx := []float64{100, 100}
	tx := []float64{50, 50}

	b := New(40, 6)
	DrawSplitGraph(b, SplitGraph{
		RxSamples: rx,
		TxSamples: tx,
		Symbols:   GraphBraille,
		Format:    RateShort,
	}, false)
	out := b.String()

	// Top line has Rx peak, bottom line has Tx peak, center has 0
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("rendered %d lines, want 6", len(lines))
	}
	if !strings.Contains(lines[0], "100") && !strings.Contains(lines[0], "128") {
		t.Errorf("top row must show download peak: %s", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], "50") && !strings.Contains(lines[len(lines)-1], "64") {
		t.Errorf("bottom row must show upload peak: %s", lines[len(lines)-1])
	}
	if !strings.Contains(lines[2], "0") {
		t.Errorf("center row must show zero: %s", lines[2])
	}
}

func TestDrawSplitGraphNoClipNarrow(t *testing.T) {
	rx := []float64{100, 200}
	tx := []float64{50, 80}

	// Plot width 6 is too narrow for "200 B/s" + tick: axis must drop without clipping or panicking
	b := New(6, 6)
	DrawSplitGraph(b, SplitGraph{
		RxSamples: rx,
		TxSamples: tx,
		Symbols:   GraphBraille,
		Format:    RateShort,
	}, false)
	out := b.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, l := range lines {
		if StringWidth(l) > 6 {
			t.Fatalf("row %d width %d exceeds width 6: %q", i, StringWidth(l), l)
		}
	}
}

func TestDrawSplitGraphSymbolsModes(t *testing.T) {
	rx := []float64{10, 50, 20}
	tx := []float64{5, 25, 10}

	// Braille
	bBraille := New(30, 8)
	DrawSplitGraph(bBraille, SplitGraph{RxSamples: rx, TxSamples: tx, Symbols: GraphBraille}, false)

	// Block
	bBlock := New(30, 8)
	DrawSplitGraph(bBlock, SplitGraph{RxSamples: rx, TxSamples: tx, Symbols: GraphBlock}, false)

	// ASCII/TTY
	bASCII := New(30, 8)
	DrawSplitGraph(bASCII, SplitGraph{RxSamples: rx, TxSamples: tx, Symbols: GraphTTY}, true)

	if bBraille.String() == bBlock.String() {
		t.Fatal("braille and block output should differ")
	}
	if !strings.Contains(bASCII.String(), "|") && !strings.Contains(bASCII.String(), "+") {
		t.Fatalf("ASCII graph missing standard ticks: %s", bASCII.String())
	}
}

func TestDrawGraphSplitForwarding(t *testing.T) {
	rx := []float64{20, 40}
	tx := []float64{10, 20}
	b := New(40, 8)
	top := DrawGraph(b, Graph{
		Split:     true,
		RxSamples: rx,
		TxSamples: tx,
		Format:    RateShort,
	}, false)
	if top <= 0 {
		t.Fatalf("top = %v, want > 0", top)
	}
}
