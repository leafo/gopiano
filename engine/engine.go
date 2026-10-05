// Package engine is the sample playback engine. Everything called from the
// audio thread (Render, HandleMIDI, NoteOn...) is allocation free.
package engine

import (
	"math"

	"github.com/leafo/gopiano/cache"
	"github.com/leafo/gopiano/xrni"
)

const (
	// voices beyond the polyphony limit are kept around to fade out
	extraVoices = 32

	retriggerFadeMs = 30
	stealFadeMs     = 10
	panicFadeMs     = 5
)

// used when the instrument has no volume envelope
var defaultEnvelope = xrni.Envelope{
	Points:  []xrni.EnvPoint{{Time: 0, Value: 1}, {Time: 300, Value: 0}},
	Sustain: 0,
}

type Options struct {
	Polyphony int
	Gain      float64 // master gain, linear
	// how much quieter the bottom of each velocity layer is than its top, in
	// dB. Smooths out the steps between layers.
	VelSpreadDB float64
}

type voice struct {
	active bool
	note   int
	age    uint64
	data   []int16
	frames int
	pos    float64
	step   float64
	gain   float32

	envT     float64 // ms into the envelope
	envLevel float32
	released bool // envelope is past the sustain point
	pedaled  bool // key is up but held by the sustain pedal
	keyDown  bool

	killing  bool
	killGain float32
	killStep float32 // per frame
}

type Engine struct {
	bank      *cache.Bank
	rate      float64
	msPerFrm  float64
	env       *xrni.Envelope
	envEnd    float64
	keymap    [128][128]int16 // note, velocity -> zone index, -1 for none
	voices    []voice
	polyphony int
	gain      float32
	velSpread float64
	pedal     bool
	ageCount  uint64
}

func New(bank *cache.Bank, outputRate int, opts Options) *Engine {
	if opts.Polyphony <= 0 {
		opts.Polyphony = 96
	}

	e := &Engine{
		bank:      bank,
		rate:      float64(outputRate),
		msPerFrm:  1000 / float64(outputRate),
		env:       bank.Envelope,
		voices:    make([]voice, opts.Polyphony+extraVoices),
		polyphony: opts.Polyphony,
		gain:      float32(opts.Gain),
		velSpread: opts.VelSpreadDB,
	}

	if e.env == nil || len(e.env.Points) == 0 {
		e.env = &defaultEnvelope
	}
	e.envEnd = e.env.Points[len(e.env.Points)-1].Time

	for n := range e.keymap {
		for v := range e.keymap[n] {
			e.keymap[n][v] = -1
		}
	}
	for i, z := range bank.Zones {
		for n := max(z.NoteLo, 0); n <= min(z.NoteHi, 127); n++ {
			for v := max(z.VelLo, 0); v <= min(z.VelHi, 127); v++ {
				e.keymap[n][v] = int16(i)
			}
		}
	}

	return e
}

// envelope value at t ms, linear between points
func (e *Engine) envAt(t float64) float32 {
	pts := e.env.Points
	if t <= pts[0].Time {
		return float32(pts[0].Value)
	}
	for i := 1; i < len(pts); i++ {
		if t < pts[i].Time {
			a, b := pts[i-1], pts[i]
			f := (t - a.Time) / (b.Time - a.Time)
			return float32(a.Value + (b.Value-a.Value)*f)
		}
	}
	return float32(pts[len(pts)-1].Value)
}

func (e *Engine) NoteOn(note, vel int) {
	if vel == 0 {
		e.NoteOff(note)
		return
	}
	if note < 0 || note > 127 || vel > 127 {
		return
	}
	zi := e.keymap[note][vel]
	if zi < 0 {
		return
	}
	z := &e.bank.Zones[zi]

	sounding := 0
	for i := range e.voices {
		v := &e.voices[i]
		if !v.active || v.killing {
			continue
		}
		if v.note == note {
			v.kill(e.rate, retriggerFadeMs)
			continue
		}
		sounding++
	}

	if sounding >= e.polyphony {
		e.stealOldest()
	}

	v := e.freeVoice()

	gain := z.Volume
	if e.velSpread > 0 && z.VelHi > z.VelLo {
		// 0 at the top of the layer's range, 1 at the bottom
		f := float64(z.VelHi-vel) / float64(z.VelHi-z.VelLo)
		gain *= math.Pow(10, -e.velSpread*f/20)
	}

	e.ageCount++
	*v = voice{
		active:   true,
		note:     note,
		age:      e.ageCount,
		data:     z.Samples(e.bank),
		frames:   int(z.Frames),
		step:     math.Exp2(float64(note-z.BaseNote)/12) * float64(e.bank.SampleRate) / e.rate,
		gain:     float32(gain),
		envLevel: e.envAt(0),
		keyDown:  true,
	}
}

func (e *Engine) NoteOff(note int) {
	for i := range e.voices {
		v := &e.voices[i]
		if !v.active || v.note != note || !v.keyDown {
			continue
		}
		v.keyDown = false
		if e.pedal {
			v.pedaled = true
		} else {
			v.released = true
		}
	}
}

func (e *Engine) SetPedal(down bool) {
	e.pedal = down
	if down {
		return
	}
	for i := range e.voices {
		v := &e.voices[i]
		if v.active && v.pedaled {
			v.pedaled = false
			v.released = true
		}
	}
}

// AllNotesOff fades out everything immediately
func (e *Engine) AllNotesOff() {
	e.pedal = false
	for i := range e.voices {
		if e.voices[i].active {
			e.voices[i].kill(e.rate, panicFadeMs)
		}
	}
}

// HandleMIDI processes one raw MIDI message, channel is ignored
func (e *Engine) HandleMIDI(msg []byte) {
	if len(msg) < 2 {
		return
	}
	switch msg[0] & 0xF0 {
	case 0x90:
		if len(msg) >= 3 {
			e.NoteOn(int(msg[1]), int(msg[2]))
		}
	case 0x80:
		e.NoteOff(int(msg[1]))
	case 0xB0:
		if len(msg) < 3 {
			return
		}
		switch msg[1] {
		case 64: // sustain pedal
			e.SetPedal(msg[2] >= 64)
		case 120, 123: // all sound off, all notes off
			e.AllNotesOff()
		}
	}
}

func (e *Engine) stealOldest() {
	var best *voice
	for i := range e.voices {
		v := &e.voices[i]
		if !v.active || v.killing {
			continue
		}
		// prefer voices that are already fading out
		if best == nil ||
			(v.released && !best.released) ||
			(v.released == best.released && v.age < best.age) {
			best = v
		}
	}
	if best != nil {
		best.kill(e.rate, stealFadeMs)
	}
}

func (e *Engine) freeVoice() *voice {
	var quietest *voice
	for i := range e.voices {
		v := &e.voices[i]
		if !v.active {
			return v
		}
		if v.killing && (quietest == nil || v.killGain < quietest.killGain) {
			quietest = v
		}
	}
	if quietest != nil {
		return quietest
	}
	// can only happen with tiny polyphony, take the oldest
	oldest := &e.voices[0]
	for i := range e.voices {
		if e.voices[i].age < oldest.age {
			oldest = &e.voices[i]
		}
	}
	return oldest
}

func (v *voice) kill(rate float64, ms float64) {
	if v.killing {
		return
	}
	v.killing = true
	v.killGain = 1
	v.killStep = float32(1000 / (ms * rate))
}

// ActiveVoices is the number of voices currently producing sound
func (e *Engine) ActiveVoices() int {
	n := 0
	for i := range e.voices {
		if e.voices[i].active {
			n++
		}
	}
	return n
}

// Render overwrites left and right (same length) with the next block of audio
func (e *Engine) Render(left, right []float32) {
	clear(left)
	clear(right)
	for i := range e.voices {
		if e.voices[i].active {
			e.renderVoice(&e.voices[i], left, right)
		}
	}
	for i := range left {
		left[i] = clamp(left[i] * e.gain)
		right[i] = clamp(right[i] * e.gain)
	}
}

func (e *Engine) renderVoice(v *voice, left, right []float32) {
	n := len(left)

	// the envelope is evaluated per block and ramped linearly across it
	t := v.envT + float64(n)*e.msPerFrm
	sustain := e.env.Sustain
	if !v.released && sustain >= 0 && t > sustain {
		t = max(sustain, v.envT)
	}
	envStart := v.envLevel
	envEnd := e.envAt(t)
	envStep := (envEnd - envStart) / float32(n)
	v.envT = t
	v.envLevel = envEnd

	g := v.gain * envStart
	gStep := v.gain * envStep

	data := v.data
	frames := v.frames
	pos := v.pos
	step := v.step

	for i := range n {
		idx := int(pos)
		if idx >= frames {
			v.active = false
			return
		}
		f := float32(pos - float64(idx))

		var l, r float32
		if idx >= 1 && idx+2 < frames {
			d := data[(idx-1)*2 : (idx+3)*2]
			l = hermite(f, d[0], d[2], d[4], d[6])
			r = hermite(f, d[1], d[3], d[5], d[7])
		} else {
			l = hermite(f, at(data, frames, idx-1, 0), at(data, frames, idx, 0), at(data, frames, idx+1, 0), at(data, frames, idx+2, 0))
			r = hermite(f, at(data, frames, idx-1, 1), at(data, frames, idx, 1), at(data, frames, idx+1, 1), at(data, frames, idx+2, 1))
		}

		amp := g
		if v.killing {
			amp *= v.killGain
			v.killGain -= v.killStep
			if v.killGain <= 0 {
				v.active = false
				return
			}
		}

		left[i] += l * amp
		right[i] += r * amp
		g += gStep
		pos += step
	}
	v.pos = pos

	if v.released && v.envT >= e.envEnd && v.envLevel <= 0 {
		v.active = false
	}
}

func at(data []int16, frames, idx, ch int) int16 {
	if idx < 0 || idx >= frames {
		return 0
	}
	return data[idx*2+ch]
}

const int16Scale = 1.0 / 32768

// 4 point cubic hermite interpolation between y1 and y2
func hermite(f float32, y0, y1, y2, y3 int16) float32 {
	a, b, c, d := float32(y0), float32(y1), float32(y2), float32(y3)
	c0 := b
	c1 := 0.5 * (c - a)
	c2 := a - 2.5*b + 2*c - 0.5*d
	c3 := 0.5*(d-a) + 1.5*(b-c)
	return (((c3*f+c2)*f+c1)*f + c0) * int16Scale
}

func clamp(x float32) float32 {
	if x > 1 {
		return 1
	}
	if x < -1 {
		return -1
	}
	return x
}
