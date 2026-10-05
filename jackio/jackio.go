// Package jackio is a minimal JACK client with one MIDI input and a stereo
// output. On PipeWire systems it runs through pipewire-jack.
package jackio

/*
#cgo pkg-config: jack
#include <errno.h>
#include <stdlib.h>
#include <jack/jack.h>
#include <jack/midiport.h>

extern int goProcess(jack_nframes_t nframes, void *arg);
extern int goXrun(void *arg);
extern void goShutdown(void *arg);

static int set_callbacks(jack_client_t *c) {
	int err = jack_set_process_callback(c, (JackProcessCallback)goProcess, NULL);
	if (err) return err;
	err = jack_set_xrun_callback(c, (JackXRunCallback)goXrun, NULL);
	if (err) return err;
	jack_on_shutdown(c, (JackShutdownCallback)goShutdown, NULL);
	return 0;
}

static jack_client_t *open_client(const char *name, jack_status_t *status) {
	return jack_client_open(name, JackNoStartServer, status);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unsafe"
)

// Handler is called from the JACK realtime thread and must not allocate or
// block.
type Handler interface {
	// MIDI is called for each incoming event, in order, between Render calls
	MIDI(msg []byte)
	Render(left, right []float32)
}

type Client struct {
	client *C.jack_client_t
	midiIn *C.jack_port_t
	outL   *C.jack_port_t
	outR   *C.jack_port_t

	handler Handler
	event   C.jack_midi_event_t // reused so cgo calls don't allocate

	Xruns    atomic.Int64
	Shutdown chan struct{}
}

// JACK gives callbacks no Go-safe way to carry a pointer, and we only ever
// have one client, so it lives here
var current *Client

func Open(name string) (*Client, error) {
	if current != nil {
		return nil, errors.New("jackio: client already open")
	}

	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var status C.jack_status_t
	client := C.open_client(cname, &status)
	if client == nil {
		return nil, fmt.Errorf("jackio: could not connect to JACK server (status 0x%x), is pipewire-jack or jackd running?", int(status))
	}

	c := &Client{client: client, Shutdown: make(chan struct{})}

	var err error
	if c.midiIn, err = c.register("midi_in", C.JACK_DEFAULT_MIDI_TYPE, C.JackPortIsInput); err != nil {
		c.Close()
		return nil, err
	}
	if c.outL, err = c.register("out_L", C.JACK_DEFAULT_AUDIO_TYPE, C.JackPortIsOutput); err != nil {
		c.Close()
		return nil, err
	}
	if c.outR, err = c.register("out_R", C.JACK_DEFAULT_AUDIO_TYPE, C.JackPortIsOutput); err != nil {
		c.Close()
		return nil, err
	}

	current = c
	return c, nil
}

func (c *Client) register(name, portType string, flags C.ulong) (*C.jack_port_t, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	ctype := C.CString(portType)
	defer C.free(unsafe.Pointer(ctype))

	port := C.jack_port_register(c.client, cname, ctype, flags, 0)
	if port == nil {
		return nil, fmt.Errorf("jackio: could not register port %s", name)
	}
	return port, nil
}

func (c *Client) SampleRate() int {
	return int(C.jack_get_sample_rate(c.client))
}

func (c *Client) BufferSize() int {
	return int(C.jack_get_buffer_size(c.client))
}

func (c *Client) Activate(h Handler) error {
	c.handler = h
	if C.set_callbacks(c.client) != 0 {
		return errors.New("jackio: could not set callbacks")
	}
	if C.jack_activate(c.client) != 0 {
		return errors.New("jackio: could not activate client")
	}
	return nil
}

// ConnectMIDI connects every hardware MIDI output whose name contains match
// (case insensitive, empty matches all) to our input. Returns the port names
// connected.
func (c *Client) ConnectMIDI(match string) []string {
	var connected []string
	for _, src := range c.ports(C.JACK_DEFAULT_MIDI_TYPE, C.JackPortIsOutput|C.JackPortIsPhysical) {
		if !containsFold(src, match) {
			continue
		}
		if c.connect(src, c.portName(c.midiIn)) {
			connected = append(connected, src)
		}
	}
	return connected
}

// ConnectAudio connects our outputs to the first two hardware playback ports
// whose names contain match. Returns the port names connected.
func (c *Client) ConnectAudio(match string) []string {
	var dests []string
	for _, p := range c.ports(C.JACK_DEFAULT_AUDIO_TYPE, C.JackPortIsInput|C.JackPortIsPhysical) {
		if containsFold(p, match) {
			dests = append(dests, p)
		}
	}
	if len(dests) == 0 {
		return nil
	}
	if len(dests) == 1 {
		dests = append(dests, dests[0])
	}

	var connected []string
	for i, src := range []*C.jack_port_t{c.outL, c.outR} {
		if c.connect(c.portName(src), dests[i]) {
			connected = append(connected, dests[i])
		}
	}
	return connected
}

func (c *Client) ports(portType string, flags C.ulong) []string {
	ctype := C.CString(portType)
	defer C.free(unsafe.Pointer(ctype))

	list := C.jack_get_ports(c.client, nil, ctype, flags)
	if list == nil {
		return nil
	}
	defer C.jack_free(unsafe.Pointer(list))

	var out []string
	for _, p := range unsafe.Slice(list, 1<<16) {
		if p == nil {
			break
		}
		out = append(out, C.GoString(p))
	}
	return out
}

func (c *Client) portName(p *C.jack_port_t) string {
	return C.GoString(C.jack_port_name(p))
}

func (c *Client) connect(src, dst string) bool {
	csrc := C.CString(src)
	defer C.free(unsafe.Pointer(csrc))
	cdst := C.CString(dst)
	defer C.free(unsafe.Pointer(cdst))

	err := C.jack_connect(c.client, csrc, cdst)
	return err == 0 || err == C.EEXIST
}

func (c *Client) Close() error {
	if c.client != nil {
		C.jack_client_close(c.client)
		c.client = nil
	}
	if current == c {
		current = nil
	}
	return nil
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

//export goProcess
func goProcess(nframes C.jack_nframes_t, _ unsafe.Pointer) C.int {
	c := current
	if c == nil {
		return 0
	}
	n := int(nframes)

	left := unsafe.Slice((*float32)(C.jack_port_get_buffer(c.outL, nframes)), n)
	right := unsafe.Slice((*float32)(C.jack_port_get_buffer(c.outR, nframes)), n)
	midi := C.jack_port_get_buffer(c.midiIn, nframes)

	// render up to each event's frame offset so note timing is sample accurate
	pos := 0
	count := C.jack_midi_get_event_count(midi)
	for i := C.uint32_t(0); i < count; i++ {
		if C.jack_midi_event_get(&c.event, midi, i) != 0 {
			continue
		}
		t := min(int(c.event.time), n)
		if t > pos {
			c.handler.Render(left[pos:t], right[pos:t])
			pos = t
		}
		c.handler.MIDI(unsafe.Slice((*byte)(c.event.buffer), int(c.event.size)))
	}
	if pos < n {
		c.handler.Render(left[pos:], right[pos:])
	}
	return 0
}

//export goXrun
func goXrun(_ unsafe.Pointer) C.int {
	if c := current; c != nil {
		c.Xruns.Add(1)
	}
	return 0
}

//export goShutdown
func goShutdown(_ unsafe.Pointer) {
	if c := current; c != nil {
		close(c.Shutdown)
	}
}
