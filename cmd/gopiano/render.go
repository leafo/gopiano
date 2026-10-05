package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/leafo/gopiano/cache"
	"github.com/leafo/gopiano/engine"
)

type testEvent struct {
	at  float64 // seconds
	msg []byte
}

func on(at float64, note, vel byte) testEvent { return testEvent{at, []byte{0x90, note, vel}} }
func off(at float64, note byte) testEvent     { return testEvent{at, []byte{0x80, note, 0}} }
func pedal(at float64, down bool) testEvent {
	v := byte(0)
	if down {
		v = 127
	}
	return testEvent{at, []byte{0xB0, 64, v}}
}

// a velocity ramp, a pedaled chord, a fast repeated note and a low/high sweep
func testSequence() []testEvent {
	var ev []testEvent
	t := 0.0

	for i, vel := range []byte{10, 30, 50, 70, 90, 110, 127} {
		note := byte(60 + []int{0, 2, 4, 5, 7, 9, 11}[i])
		ev = append(ev, on(t, note, vel), off(t+0.4, note))
		t += 0.5
	}

	t += 0.5
	ev = append(ev, pedal(t, true))
	for _, n := range []byte{48, 55, 60, 64, 67} {
		ev = append(ev, on(t, n, 80), off(t+0.3, n))
	}
	ev = append(ev, pedal(t+3, false))
	t += 4

	for range 8 {
		ev = append(ev, on(t, 72, 90), off(t+0.08, 72))
		t += 0.1
	}
	t += 0.5

	for n := byte(21); n <= 108; n += 5 {
		ev = append(ev, on(t, n, 70), off(t+0.15, n))
		t += 0.12
	}
	return ev
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	path := fs.String("cache", cache.DefaultPath(), "cache file from import")
	out := fs.String("o", "gopiano-test.wav", "output WAV file")
	rate := fs.Int("rate", 48000, "output sample rate")
	gain := fs.Float64("gain", 0.5, "master gain")
	fs.Parse(args)

	bank, err := cache.Load(*path)
	if err != nil {
		return err
	}
	defer bank.Close()

	eng := engine.New(bank, *rate, engine.Options{Gain: *gain, VelSpreadDB: 3})
	events := testSequence()
	length := events[len(events)-1].at + 3

	const block = 64
	total := int(length * float64(*rate))
	left := make([]float32, block)
	right := make([]float32, block)
	pcm := make([]int16, 0, total*2)

	start := time.Now()
	next := 0
	for frame := 0; frame < total; frame += block {
		for next < len(events) && int(events[next].at*float64(*rate)) <= frame {
			eng.HandleMIDI(events[next].msg)
			next++
		}
		eng.Render(left, right)
		for i := range block {
			pcm = append(pcm, toInt16(left[i]), toInt16(right[i]))
		}
	}
	elapsed := time.Since(start)

	if err := writeWAV(*out, *rate, pcm); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %.1fs of audio rendered in %s (%.0fx realtime)\n",
		*out, length, elapsed.Round(time.Millisecond), length/elapsed.Seconds())
	return nil
}

func toInt16(x float32) int16 {
	return int16(math.Round(float64(x) * 32767))
}

func writeWAV(path string, rate int, pcm []int16) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	dataLen := uint32(len(pcm) * 2)
	le := binary.LittleEndian

	w.WriteString("RIFF")
	binary.Write(w, le, 36+dataLen)
	w.WriteString("WAVEfmt ")
	binary.Write(w, le, uint32(16))
	binary.Write(w, le, uint16(1)) // PCM
	binary.Write(w, le, uint16(2)) // channels
	binary.Write(w, le, uint32(rate))
	binary.Write(w, le, uint32(rate*4)) // byte rate
	binary.Write(w, le, uint16(4))      // block align
	binary.Write(w, le, uint16(16))     // bits per sample
	w.WriteString("data")
	binary.Write(w, le, dataLen)
	binary.Write(w, le, pcm)

	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}
