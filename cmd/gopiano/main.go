package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/leafo/gopiano/cache"
	"github.com/leafo/gopiano/engine"
	"github.com/leafo/gopiano/jackio"
	"github.com/leafo/gopiano/xrni"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage: gopiano [command] [flags]

commands:
  play               play the imported piano from MIDI input (default)
  import FILE.xrni   decode a Renoise instrument into the cache
  render             render a test sequence to a WAV file

run "gopiano COMMAND -h" for flags
`)
}

func main() {
	args := os.Args[1:]
	cmd := "play"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "play":
		err = cmdPlay(args)
	case "import":
		err = cmdImport(args)
	case "render":
		err = cmdRender(args)
	case "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "gopiano:", err)
		os.Exit(1)
	}
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	out := fs.String("o", cache.DefaultPath(), "cache file to write")
	layers := fs.Int("layers", 8, "velocity layers to keep (0 = all)")
	maxLen := fs.Float64("maxlen", 6, "max sample length in seconds (0 = no limit)")
	fade := fs.Float64("fade", 1, "fade out length for truncated samples, seconds")
	silence := fs.Float64("silence", -72, "trim trailing audio quieter than this, dB")
	fs.Parse(args)

	if fs.NArg() != 1 {
		return fmt.Errorf("import: expected one .xrni file")
	}

	start := time.Now()
	inst, err := xrni.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer inst.Close()

	header, err := cache.Build(inst, *out, cache.BuildOptions{
		Layers:    *layers,
		MaxLen:    *maxLen,
		FadeLen:   *fade,
		SilenceDB: *silence,
		OnProgress: func(done, total int) {
			fmt.Fprintf(os.Stderr, "\rdecoding %d/%d", done, total)
		},
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}

	fmt.Printf("%s: %d zones, %d Hz, %.0f MB, took %s\n",
		header.Name, len(header.Zones), header.SampleRate,
		float64(header.DataFrames*4)/1e6, time.Since(start).Round(time.Millisecond))
	fmt.Println("wrote", *out)
	return nil
}

func cmdPlay(args []string) error {
	start := time.Now()

	fs := flag.NewFlagSet("play", flag.ExitOnError)
	path := fs.String("cache", cache.DefaultPath(), "cache file from import")
	midiMatch := fs.String("midi", "", "only connect MIDI inputs whose name contains this")
	outMatch := fs.String("out", "", "only connect to audio outputs whose name contains this")
	latency := fs.Int("latency", 128, "buffer size in frames to force on PipeWire while running (0 = leave as is)")
	gain := fs.Float64("gain", 0.5, "master gain")
	poly := fs.Int("poly", 96, "max simultaneous voices")
	velSpread := fs.Float64("velspread", 3, "dB of volume range within each velocity layer")
	fs.Parse(args)

	// pipewire-jack ignores PIPEWIRE_LATENCY, this forces the graph quantum
	// while we're connected
	if *latency > 0 && os.Getenv("PIPEWIRE_QUANTUM") == "" {
		os.Setenv("PIPEWIRE_QUANTUM", fmt.Sprintf("%d/48000", *latency))
	}

	bank, err := cache.Load(*path)
	if os.IsNotExist(err) {
		return fmt.Errorf("no cache at %s, run: gopiano import FILE.xrni", *path)
	}
	if err != nil {
		return err
	}
	defer bank.Close()

	client, err := jackio.Open("gopiano")
	if err != nil {
		return err
	}
	defer client.Close()

	eng := engine.New(bank, client.SampleRate(), engine.Options{
		Polyphony:   *poly,
		Gain:        *gain,
		VelSpreadDB: *velSpread,
	})

	// the audio thread allocates nothing and the sample data is outside the
	// Go heap, so turning the GC off just removes any chance of a pause
	debug.SetGCPercent(-1)

	if err := client.Activate(handler{eng}); err != nil {
		return err
	}

	midiPorts := client.ConnectMIDI(*midiMatch)
	audioPorts := client.ConnectAudio(*outMatch)

	fmt.Printf("ready in %s: %s, %d Hz, %d frame buffer (%.1f ms)\n",
		time.Since(start).Round(time.Millisecond), bank.Name,
		client.SampleRate(), client.BufferSize(),
		1000*float64(client.BufferSize())/float64(client.SampleRate()))
	fmt.Printf("midi in:   %s\n", listOrNone(midiPorts))
	fmt.Printf("audio out: %s\n", listOrNone(audioPorts))

	// pull the sample data into RAM now so notes never wait on the page cache
	go func() {
		if err := bank.Lock(); err != nil {
			fmt.Fprintf(os.Stderr, "note: samples are preloaded but not pinned in RAM (mlock: %v)\n", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastXruns int64
	for {
		select {
		case <-sig:
			return nil
		case <-client.Shutdown:
			return fmt.Errorf("JACK server shut down")
		case <-tick.C:
			if x := client.Xruns.Load(); x != lastXruns {
				fmt.Fprintf(os.Stderr, "xruns: %d (try a larger -latency)\n", x)
				lastXruns = x
			}
		}
	}
}

type handler struct {
	eng *engine.Engine
}

func (h handler) MIDI(msg []byte)              { h.eng.HandleMIDI(msg) }
func (h handler) Render(left, right []float32) { h.eng.Render(left, right) }

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}
