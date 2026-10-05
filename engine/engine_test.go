package engine

import (
	"math"
	"os"
	"testing"

	"github.com/leafo/gopiano/cache"
)

func loadBank(t *testing.T) *cache.Bank {
	bank, err := cache.Load(cache.DefaultPath())
	if os.IsNotExist(err) {
		t.Skip("no imported cache, run gopiano import first")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bank.Close() })
	return bank
}

// strongest frequency between lo and hi Hz, by brute force DFT
func peakFreq(x []float32, rate, lo, hi float64) float64 {
	best, bestMag := 0.0, 0.0
	for f := lo; f <= hi; f += 0.25 {
		var re, im float64
		w := 2 * math.Pi * f / rate
		for i, v := range x {
			re += float64(v) * math.Cos(w*float64(i))
			im += float64(v) * math.Sin(w*float64(i))
		}
		if m := re*re + im*im; m > bestMag {
			best, bestMag = f, m
		}
	}
	return best
}

func TestPitch(t *testing.T) {
	bank := loadBank(t)
	const rate = 48000

	// A4 is a sampled note, A#4 and G#4 are shifted from it
	for _, note := range []int{68, 69, 70} {
		e := New(bank, rate, Options{Gain: 1})
		e.NoteOn(note, 80)

		left := make([]float32, rate/2)
		right := make([]float32, rate/2)
		e.Render(left, right)

		want := 440 * math.Exp2(float64(note-69)/12)
		got := peakFreq(left[rate/10:], rate, want*0.95, want*1.05)
		if math.Abs(got-want) > 1.5 {
			t.Errorf("note %d: got %.2f Hz, want %.2f Hz", note, got, want)
		}
	}
}

func TestNoAllocs(t *testing.T) {
	bank := loadBank(t)
	e := New(bank, 48000, Options{Gain: 0.5})
	left := make([]float32, 64)
	right := make([]float32, 64)
	on := []byte{0x90, 60, 100}
	off := []byte{0x80, 60, 0}

	allocs := testing.AllocsPerRun(200, func() {
		e.HandleMIDI(on)
		e.Render(left, right)
		e.HandleMIDI(off)
		e.Render(left, right)
	})
	if allocs != 0 {
		t.Errorf("audio path allocated %v times per run", allocs)
	}
}

func TestPedalAndRelease(t *testing.T) {
	bank := loadBank(t)
	e := New(bank, 48000, Options{Gain: 0.5})
	buf := make([]float32, 48000)
	buf2 := make([]float32, 48000)

	e.SetPedal(true)
	e.NoteOn(60, 100)
	e.NoteOff(60)
	e.Render(buf, buf2)
	if e.ActiveVoices() != 1 {
		t.Fatalf("pedal should hold the note, %d voices", e.ActiveVoices())
	}

	e.SetPedal(false)
	e.Render(buf, buf2)
	if e.ActiveVoices() != 0 {
		t.Fatalf("note should end after pedal release, %d voices", e.ActiveVoices())
	}
}

func TestPolyphonyLimit(t *testing.T) {
	bank := loadBank(t)
	e := New(bank, 48000, Options{Polyphony: 8, Gain: 0.5})
	buf := make([]float32, 64)
	buf2 := make([]float32, 64)
	for n := 30; n < 90; n++ {
		e.NoteOn(n, 90)
		e.Render(buf, buf2)
	}
	// stolen voices fade out within 10ms
	for range 20 {
		e.Render(buf, buf2)
	}
	if v := e.ActiveVoices(); v > 8 {
		t.Errorf("%d voices active, limit is 8", v)
	}
}
