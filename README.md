# gopiano

A small sampler for playing a piano from a MIDI keyboard. It loads a Renoise
instrument (`.xrni`), plays it through JACK (pipewire-jack works), and starts
in about 10ms. I wanted something I could open quickly to play for a few
minutes without starting a DAW.

I've only used it with the Salamander Grand Piano `.xrni`. Other Renoise
instruments might work if they're plain multisamples with a volume envelope,
but I haven't tried any.

## Building

Needs Go and the JACK headers (`pipewire-jack` or `jack2` on Arch).

```sh
go build -o gopiano ./cmd/gopiano
```

## Usage

First, import the instrument. This decodes the FLAC samples into a cache file
at `~/.cache/gopiano/piano.bin`, which takes about a second:

```sh
./gopiano import "Salamander Grand Piano.xrni"
```

To save memory, the import keeps 8 of the 16 velocity layers and cuts each
sample to at most 6 seconds, which comes to about 260MB. To keep more:

```sh
./gopiano import -layers 16 -maxlen 10 "Salamander Grand Piano.xrni"
```

Then run it:

```sh
./gopiano
```

It connects to every hardware MIDI input and the first hardware audio output
it finds. Use `-midi` and `-out` to match port names if that picks the wrong
ones.

Useful flags for `play`:

- `-latency 128`: buffer size in frames. On PipeWire this sets the buffer
  size for everything else running too, as long as gopiano is open. Try 64;
  if you get xruns, go back up.
- `-gain 0.5`: master volume.
- `-poly 96`: maximum number of voices playing at once.
- `-velspread 3`: dB of volume difference within each velocity layer, to
  smooth out the steps between layers. 0 turns it off.

`./gopiano render` plays a short test sequence into a WAV file, so you can
check the engine without a keyboard.

## Notes

- Notes start at the exact sample position of their MIDI event within each
  buffer.
- The engine doesn't allocate memory in the audio callback, and the garbage
  collector is turned off once the instrument is loaded.
- The sample data is memory mapped and loaded into RAM at startup. It tries
  to `mlock` it too, but the default memlock limit is usually too small for
  that to work.
- Renoise envelope times are read as milliseconds. That gives a release that
  sounds right, but I haven't confirmed it against Renoise.

## Tests

```sh
go test ./...
```

The engine tests need the imported cache file; without it they're skipped.
