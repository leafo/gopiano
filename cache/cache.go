// Package cache stores a trimmed, pre-decoded copy of an instrument in a
// single file that can be memory mapped for instant startup.
//
// File layout: 8 byte magic, uint32 little endian header length, JSON header,
// zero padding up to a page boundary, then interleaved stereo int16 sample
// data for all zones back to back.
package cache

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"unsafe"

	"github.com/leafo/gopiano/xrni"
	"golang.org/x/sys/unix"
)

const (
	magic     = "GOPIANO\x01"
	pageAlign = 4096
)

type Zone struct {
	Name     string  `json:"name"`
	BaseNote int     `json:"base_note"`
	NoteLo   int     `json:"note_lo"`
	NoteHi   int     `json:"note_hi"`
	VelLo    int     `json:"vel_lo"`
	VelHi    int     `json:"vel_hi"`
	Volume   float64 `json:"volume"`
	Offset   int64   `json:"offset"` // in frames from start of data
	Frames   int64   `json:"frames"`
}

type Header struct {
	Name       string         `json:"name"`
	SampleRate int            `json:"sample_rate"`
	Layers     int            `json:"layers"`
	MaxLen     float64        `json:"max_len"` // seconds
	Envelope   *xrni.Envelope `json:"envelope"`
	Zones      []Zone         `json:"zones"`
	DataOffset int64          `json:"data_offset"`
	DataFrames int64          `json:"data_frames"`
}

type Bank struct {
	Header
	Data []int16 // interleaved stereo, backed by the mmapped file

	mapped []byte
}

type BuildOptions struct {
	Layers     int     // velocity layers to keep, <= 0 keeps all
	MaxLen     float64 // max sample length in seconds, <= 0 for no limit
	FadeLen    float64 // fade applied to the end of truncated samples, seconds
	SilenceDB  float64 // trailing audio quieter than this is trimmed
	OnProgress func(done, total int)
}

func DefaultPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "gopiano", "piano.bin")
}

// pickLayers keeps n of the instrument's velocity layers, spread evenly with a
// bias towards the louder layer of each group, and widens each kept layer's
// velocity range so the full 0-127 range stays covered.
func pickLayers(samples []*xrni.Sample, n int) []*xrni.Sample {
	var starts []int
	for _, s := range samples {
		if !slices.Contains(starts, s.VelLo) {
			starts = append(starts, s.VelLo)
		}
	}
	slices.Sort(starts)

	total := len(starts)
	if n <= 0 || n >= total {
		return samples
	}

	// kept layer index -> new velocity range
	type velRange struct{ lo, hi int }
	keep := map[int]velRange{}
	for i := range n {
		src := (i+1)*total/n - 1
		keep[starts[src]] = velRange{i * 128 / n, (i+1)*128/n - 1}
	}

	var out []*xrni.Sample
	for _, s := range samples {
		r, ok := keep[s.VelLo]
		if !ok {
			continue
		}
		c := *s
		c.VelLo, c.VelHi = r.lo, r.hi
		out = append(out, &c)
	}
	return out
}

type decoded struct {
	data []int16
	rate int
	err  error
}

func Build(inst *xrni.Instrument, path string, opts BuildOptions) (*Header, error) {
	samples := pickLayers(inst.Samples, opts.Layers)
	results := make([]decoded, len(samples))

	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for range runtime.NumCPU() {
		wg.Go(func() {
			for i := range jobs {
				results[i] = decodeSample(samples[i], opts)
				if opts.OnProgress != nil {
					mu.Lock()
					done++
					opts.OnProgress(done, len(samples))
					mu.Unlock()
				}
			}
		})
	}
	for i := range samples {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	header := Header{
		Name:     inst.Name,
		Layers:   opts.Layers,
		MaxLen:   opts.MaxLen,
		Envelope: inst.Envelope,
	}

	var offset int64
	for i, s := range samples {
		res := results[i]
		if res.err != nil {
			return nil, res.err
		}
		if header.SampleRate == 0 {
			header.SampleRate = res.rate
		} else if header.SampleRate != res.rate {
			return nil, fmt.Errorf("%s: sample rate %d differs from %d", s.Name, res.rate, header.SampleRate)
		}

		frames := int64(len(res.data) / 2)
		header.Zones = append(header.Zones, Zone{
			Name:     s.Name,
			BaseNote: s.BaseNote,
			NoteLo:   s.NoteLo,
			NoteHi:   s.NoteHi,
			VelLo:    s.VelLo,
			VelHi:    s.VelHi,
			Volume:   s.Volume,
			Offset:   offset,
			Frames:   frames,
		})
		offset += frames
	}
	header.DataFrames = offset

	// the header length depends on DataOffset, so encode once to size it
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	header.DataOffset = alignUp(int64(len(magic)+4+len(headerJSON)+32), pageAlign)
	headerJSON, err = json.Marshal(header)
	if err != nil {
		return nil, err
	}

	var prefix bytes.Buffer
	prefix.WriteString(magic)
	binary.Write(&prefix, binary.LittleEndian, uint32(len(headerJSON)))
	prefix.Write(headerJSON)
	if int64(prefix.Len()) > header.DataOffset {
		return nil, fmt.Errorf("cache: header larger than reserved space")
	}
	prefix.Write(make([]byte, header.DataOffset-int64(prefix.Len())))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)

	if _, err := f.Write(prefix.Bytes()); err != nil {
		f.Close()
		return nil, err
	}
	for _, res := range results {
		if err := binary.Write(f, binary.LittleEndian, res.data); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}

	return &header, nil
}

func decodeSample(s *xrni.Sample, opts BuildOptions) decoded {
	// decode a little past the limit so we can tell if the sample was cut
	maxSeconds := 0.0
	if opts.MaxLen > 0 {
		maxSeconds = opts.MaxLen + 0.01
	}

	data, rate, err := s.Decode(maxSeconds)
	if err != nil {
		return decoded{err: err}
	}

	truncated := false
	if opts.MaxLen > 0 {
		limit := int(opts.MaxLen * float64(rate))
		if len(data)/2 > limit {
			data = data[:limit*2]
			truncated = true
		}
	}

	// trim trailing near-silence
	threshold := int16(32768 * dbToGain(opts.SilenceDB))
	end := len(data) / 2
	for end > 0 && abs16(data[end*2-2]) <= threshold && abs16(data[end*2-1]) <= threshold {
		end--
	}
	if end < len(data)/2 {
		data = data[:end*2]
		truncated = false
	}

	fadeLen := int(0.005 * float64(rate))
	if truncated {
		fadeLen = int(opts.FadeLen * float64(rate))
	}
	fadeOut(data, fadeLen)

	return decoded{data: slices.Clip(data), rate: rate}
}

func fadeOut(data []int16, frames int) {
	n := len(data) / 2
	frames = min(frames, n)
	for i := range frames {
		// i counts back from the last frame, gain goes 0 -> 1
		g := float64(i) / float64(frames)
		g *= g
		idx := (n - 1 - i) * 2
		data[idx] = int16(float64(data[idx]) * g)
		data[idx+1] = int16(float64(data[idx+1]) * g)
	}
}

// Load memory maps a cache file. The mapping is read only and pages are
// faulted in lazily; call Lock to pull everything into RAM.
func Load(path string) (*Bank, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	mapped, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("cache: mmap: %w", err)
	}

	bank, err := parse(mapped)
	if err != nil {
		unix.Munmap(mapped)
		return nil, fmt.Errorf("cache: %s: %w", path, err)
	}
	return bank, nil
}

func parse(mapped []byte) (*Bank, error) {
	if len(mapped) < len(magic)+4 || string(mapped[:len(magic)]) != magic {
		return nil, fmt.Errorf("not a gopiano cache file (or old version), re-run import")
	}
	headerLen := int(binary.LittleEndian.Uint32(mapped[len(magic):]))
	start := len(magic) + 4
	if start+headerLen > len(mapped) {
		return nil, fmt.Errorf("truncated header")
	}

	bank := &Bank{mapped: mapped}
	if err := json.Unmarshal(mapped[start:start+headerLen], &bank.Header); err != nil {
		return nil, fmt.Errorf("bad header: %w", err)
	}

	dataEnd := bank.DataOffset + bank.DataFrames*4
	if dataEnd > int64(len(mapped)) {
		return nil, fmt.Errorf("truncated sample data")
	}

	// sample data is little endian int16, the same as the in-memory layout
	// on the platforms we run on
	raw := mapped[bank.DataOffset:dataEnd]
	bank.Data = unsafe.Slice((*int16)(unsafe.Pointer(unsafe.SliceData(raw))), len(raw)/2)
	return bank, nil
}

// Lock faults in and pins the sample data in RAM so the audio thread never
// waits on the page cache. If locking isn't permitted it still pre-faults
// the pages, which is almost as good.
func (b *Bank) Lock() error {
	err := unix.Mlock(b.mapped)
	if err != nil {
		unix.Madvise(b.mapped, unix.MADV_WILLNEED)
		var sum byte
		for i := 0; i < len(b.mapped); i += os.Getpagesize() {
			sum += b.mapped[i]
		}
		touchSink = sum
	}
	return err
}

// keeps the page touching loop in Lock from being optimized away
var touchSink byte

func (b *Bank) Close() error {
	return unix.Munmap(b.mapped)
}

func (z *Zone) Samples(b *Bank) []int16 {
	return b.Data[z.Offset*2 : (z.Offset+z.Frames)*2]
}

func dbToGain(db float64) float64 {
	return math.Pow(10, db/20)
}

func alignUp(n, align int64) int64 {
	return (n + align - 1) / align * align
}

func abs16(v int16) int16 {
	if v < 0 {
		if v == -32768 {
			return 32767
		}
		return -v
	}
	return v
}
