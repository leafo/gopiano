// Package xrni reads Renoise instrument files (.xrni): a zip archive holding
// Instrument.xml and the sample data as FLAC files.
package xrni

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mewkiz/flac"
)

// Renoise numbers notes from C-0 = 0, MIDI from C-1 = 0.
const renoiseToMIDI = 12

type Sample struct {
	Index    int
	Name     string
	BaseNote int // MIDI note
	NoteLo   int // MIDI note, inclusive
	NoteHi   int
	VelLo    int // inclusive, 0-127
	VelHi    int
	Volume   float64
	file     *zip.File
}

type EnvPoint struct {
	Time  float64 // ms
	Value float64
}

type Envelope struct {
	Points  []EnvPoint
	Sustain float64 // ms; -1 when there is no sustain point
}

type Instrument struct {
	Name     string
	Samples  []*Sample
	Envelope *Envelope // volume envelope, nil if none

	zr *zip.ReadCloser
}

type xmlInstrument struct {
	Name    string `xml:"Name"`
	Samples []struct {
		Name    string  `xml:"Name"`
		Volume  float64 `xml:"Volume"`
		Mapping struct {
			BaseNote      int `xml:"BaseNote"`
			NoteStart     int `xml:"NoteStart"`
			NoteEnd       int `xml:"NoteEnd"`
			VelocityStart int `xml:"VelocityStart"`
			VelocityEnd   int `xml:"VelocityEnd"`
		} `xml:"Mapping"`
	} `xml:"SampleGenerator>Samples>Sample"`
	Devices []struct {
		Type            string `xml:"type,attr"`
		Target          string `xml:"Target"`
		TempoSynced     bool   `xml:"TempoSynced"`
		SustainIsActive bool   `xml:"SustainIsActive"`
		SustainPos      int    `xml:"SustainPos"`
		Points          []string `xml:"Nodes>Points>Point"`
	} `xml:"SampleGenerator>ModulationSets>ModulationSet>Devices>Device"`
}

func Open(path string) (*Instrument, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}

	inst, err := load(zr)
	if err != nil {
		zr.Close()
		return nil, err
	}
	return inst, nil
}

func load(zr *zip.ReadCloser) (*Instrument, error) {
	// sample data files are named "SampleData/Sample<index> (<name>).flac"
	files := map[int]*zip.File{}
	var xmlFile *zip.File
	for _, f := range zr.File {
		if f.Name == "Instrument.xml" {
			xmlFile = f
			continue
		}

		rest, ok := strings.CutPrefix(f.Name, "SampleData/Sample")
		if !ok {
			continue
		}
		numStr, _, _ := strings.Cut(rest, " ")
		if n, err := strconv.Atoi(numStr); err == nil {
			files[n] = f
		}
	}

	if xmlFile == nil {
		return nil, fmt.Errorf("xrni: missing Instrument.xml")
	}

	r, err := xmlFile.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var doc xmlInstrument
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("xrni: parsing Instrument.xml: %w", err)
	}

	inst := &Instrument{Name: doc.Name, zr: zr}

	for i, s := range doc.Samples {
		f := files[i]
		if f == nil {
			return nil, fmt.Errorf("xrni: no sample data for sample %d (%s)", i, s.Name)
		}
		m := s.Mapping
		inst.Samples = append(inst.Samples, &Sample{
			Index:    i,
			Name:     s.Name,
			BaseNote: m.BaseNote + renoiseToMIDI,
			NoteLo:   m.NoteStart + renoiseToMIDI,
			NoteHi:   m.NoteEnd + renoiseToMIDI,
			VelLo:    m.VelocityStart,
			VelHi:    m.VelocityEnd,
			Volume:   s.Volume,
			file:     f,
		})
	}

	for _, d := range doc.Devices {
		if d.Type != "SampleEnvelopeModulationDevice" || d.Target != "Volume" {
			continue
		}
		if d.TempoSynced {
			return nil, fmt.Errorf("xrni: tempo synced envelopes are not supported")
		}

		env := &Envelope{Sustain: -1}
		if d.SustainIsActive {
			env.Sustain = float64(d.SustainPos)
		}
		for _, p := range d.Points {
			t, v, ok := strings.Cut(p, ",")
			if !ok {
				return nil, fmt.Errorf("xrni: bad envelope point %q", p)
			}
			tf, err1 := strconv.ParseFloat(t, 64)
			vf, err2 := strconv.ParseFloat(v, 64)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("xrni: bad envelope point %q", p)
			}
			env.Points = append(env.Points, EnvPoint{Time: tf, Value: vf})
		}
		inst.Envelope = env
		break
	}

	return inst, nil
}

func (inst *Instrument) Close() error {
	return inst.zr.Close()
}

// Decode reads up to maxSeconds of the sample as interleaved stereo int16
// (mono is duplicated to both channels). maxSeconds <= 0 reads it all. Safe
// to call concurrently for different samples.
func (s *Sample) Decode(maxSeconds float64) (data []int16, sampleRate int, err error) {
	r, err := s.file.Open()
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()

	stream, err := flac.New(r)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", s.file.Name, err)
	}
	defer stream.Close()

	info := stream.Info
	if info.NChannels < 1 || info.NChannels > 2 {
		return nil, 0, fmt.Errorf("%s: unsupported channel count %d", s.file.Name, info.NChannels)
	}
	shift := int(info.BitsPerSample) - 16

	maxFrames := 0
	if maxSeconds > 0 {
		maxFrames = int(maxSeconds * float64(info.SampleRate))
	}

	total := int(info.NSamples)
	if maxFrames > 0 && (total == 0 || total > maxFrames) {
		total = maxFrames
	}
	data = make([]int16, 0, total*2)

	conv := func(v int32) int16 {
		if shift > 0 {
			return int16(v >> shift)
		}
		return int16(v << -shift)
	}

	for maxFrames <= 0 || len(data) < maxFrames*2 {
		frame, err := stream.ParseNext()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", s.file.Name, err)
		}

		left := frame.Subframes[0].Samples
		right := left
		if info.NChannels == 2 {
			right = frame.Subframes[1].Samples
		}
		for i := range left {
			if maxFrames > 0 && len(data) >= maxFrames*2 {
				break
			}
			data = append(data, conv(left[i]), conv(right[i]))
		}
	}

	return data, int(info.SampleRate), nil
}
