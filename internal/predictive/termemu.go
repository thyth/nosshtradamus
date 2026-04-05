/*
 * nosshtradamus: predictive terminal emulation for SSH
 * Copyright 2019-2026 Daniel Selifonov
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package predictive

import (
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh"
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh/overlay"
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh/parser"
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh/terminal"

	"bytes"
	"fmt"
	"io"
	"sync"
	"time"
)

// Mosh (Mobile Shell) implements a terminal emulator capable of predictive/speculative echo and line editing for
// interactive sessions. These predictions are displayed to the user effectively immediately in response to input,
// without waiting for the remote server to echo back output. The server responses are used to confirm and correct these
// predictions, but the immediate local output provides substantially better interactive user experience when these
// server responses are subject to delay.
//
// The Mosh model implements this capability on top of a UDP based state synchronization protocol, and runs terminal
// emulation state tracking on both ends. This allows skipping intermediate updates between the last state synchronized
// to the client and the current state of the server, rather than transmitting a raw terminal octet stream.
//
// The go-mosh wrapper for Mosh exposes the C++ classes implementing terminal emulation, computation of state deltas,
// and prediction. The wrapper does not expose the state synchronization protocol.

// GetVersion extracts the build-time version of mosh used to provide interposition.
func GetVersion() string {
	return mosh.GetVersion()
}

// This package implements a predictive interposer for octet streams representing interactive terminal sessions, which
// leverages the Mosh classes, for injection of responsive UX on the client side (without any requirements on server).
// While not all of Mosh's benefits are available (e.g. instant Ctrl-C), it still provides effectively immediate
// reactivity to user inputs.
//
// This interposer satisfies Go's io.ReadWriteCloser interface. The interposer wraps an upstream io.ReadWriteCloser
// (e.g. a net.Conn, or ssh.Channel). Writes to the interposer are written both to the upstream and to the predictive
// terminal state tracker. Reads from the interposer contain a combination of predictive speculations in response to
// local writes, and state read from the upstream.
//
// In addition to the upstream io.ReadWriteCloser, the predictive interposer requires a callback function parameter
// called "openEpoch". This callback will be called (asynchronously) by the interposer to designate the opening of a
// predictive epoch upon writing new data. This callback must invoke the CloseEpoch function after the written data has
// been acknowledged and reflected in the data read from upstream, designating which epoch is completed and pass through
// the timestamp it was provided as an argument. This data cannot be carried in-band in the terminal octet stream, but
// should be sent in a parallel channel that shares the same latency/throughput characteristics as the octet stream.

type Interposer struct {
	upstream        io.ReadWriteCloser
	upstreamAsynk   *Asynk
	upstreamErr     chan error
	lastUpstreamErr error
	droppedUpdate   bool

	coalesceInterval time.Duration
	lastUpdated      time.Time

	pending *bytes.Buffer

	width, height int

	bufferMutex, emulatorMutex *sync.Mutex

	completeRemoteState *terminal.Framebuffer // state of the remote terminal, in the last complete epoch
	pendingRemoteState  *terminal.Framebuffer // state of the remote terminal, as we know it currently

	localState *terminal.Framebuffer // state of the local terminal, including possible predictions
	display    *terminal.Display     // used to generate deltas between framebuffers
	emulator   *terminal.Complete    // processor of terminal control sequences

	epoch                  uint64
	pendingEpoch           bool                      // if an update is pending
	pendingEpochStarted    time.Time                 // tracking start of a pending epoch to calculate roundtrip latency
	predictor              *overlay.PredictionEngine // speculative/predictive engine
	predictionNotification chan interface{}

	openEpoch func(interposer *Interposer, epoch uint64, openedAt time.Time)

	opened, initialized bool
}

type DisplayPreference overlay.DisplayPreference

// bridge to the Mosh overlay parameters
const (
	PredictAlways       = DisplayPreference(overlay.PredictAlways)
	PredictNever        = DisplayPreference(overlay.PredictNever)
	PredictAdaptive     = DisplayPreference(overlay.PredictAdaptive)
	PredictExperimental = DisplayPreference(overlay.PredictExperimental)
)

type InterposerOptions struct {
	CoalesceInterval         time.Duration
	DisplayPreference        DisplayPreference
	DisplayPredictOverwrites bool
	SkipOpen                 bool
	SkipInitialize           bool
}

// GetDefaultInterposerOptions produces a set of reasonable defaults for the interposer's prediction and coalescing
// parameters. Customize as needed in consumers of the interposer.
func GetDefaultInterposerOptions() *InterposerOptions {
	return &InterposerOptions{
		// Specifies the time interval within which multiple updates to the terminal are coalesced into a single delta
		// by Mosh. Default is 60 frames per second (slightly higher than Mosh's default of 50 frames per second).
		CoalesceInterval: time.Second / 60,

		// Specifies the default prediction mode. Using "experimental", as it is the most aggressive.
		DisplayPreference: PredictExperimental,

		// Specifies if the prediction should prefer overwrite predictions over insertion predictions. Insertion
		// predictions tend to provide better experience for line editing.
		DisplayPredictOverwrites: false,
	}
}

func Interpose(rwc io.ReadWriteCloser, openEpoch func(interposer *Interposer, epoch uint64, openedAt time.Time),
	options *InterposerOptions) *Interposer {
	inter := &Interposer{
		upstream:      rwc,
		upstreamAsynk: MakeAsynk(rwc, 8192),
		upstreamErr:   make(chan error),

		coalesceInterval: options.CoalesceInterval,

		pending: nil,

		width:  1,
		height: 1,

		bufferMutex:   &sync.Mutex{},
		emulatorMutex: &sync.Mutex{},

		completeRemoteState: terminal.MakeFramebuffer(1, 1),
		pendingRemoteState:  terminal.MakeFramebuffer(1, 1),

		localState: terminal.MakeFramebuffer(1, 1),
		display:    terminal.MakeDisplay(true),
		emulator:   terminal.MakeComplete(1, 1),

		epoch:                  0,
		pendingEpoch:           false,
		predictor:              overlay.MakePredictionEngine(),
		predictionNotification: make(chan interface{}),

		openEpoch: openEpoch,

		opened:      options.SkipOpen,
		initialized: options.SkipInitialize,
	}
	inter.predictor.SetDisplayPreference(overlay.DisplayPreference(options.DisplayPreference))
	inter.predictor.SetPredictOverwrite(options.DisplayPredictOverwrites)
	// SetSendInterval with zero so initial predictions don't show underlined (until we get a measurement)
	inter.predictor.SetSendInterval(0)

	go inter.pullFromUpstream()
	return inter
}

func (i *Interposer) ChangeDisplayPreference(preference DisplayPreference) {
	i.emulatorMutex.Lock()
	i.predictor.SetDisplayPreference(overlay.DisplayPreference(preference))
	i.emulatorMutex.Unlock()
}

func (i *Interposer) ChangeOverwritePrediction(enabled bool) {
	i.emulatorMutex.Lock()
	i.predictor.SetPredictOverwrite(enabled)
	i.emulatorMutex.Unlock()
}

func (i *Interposer) CloseEpoch(epoch uint64, openedAt time.Time) {
	i.emulatorMutex.Lock()
	pending := i.epoch > epoch
	latency := time.Now().Sub(openedAt)
	i.predictor.LocalFrameAcked(epoch)
	i.predictor.LocalFrameLateAcked(epoch)
	i.predictor.SetSendInterval(latency)

	i.completeRemoteState = terminal.CopyFramebuffer(i.pendingRemoteState)
	i.pendingEpoch = pending
	if !pending {
		var zero time.Time
		i.pendingEpochStarted = zero
	}

	i.emulatorMutex.Unlock()

	// notify update
	select {
	case i.upstreamErr <- nil:
	default:
		i.droppedUpdate = true
	}
}

// Inject terminal data directly into the emulator to manipulate state exogenously from the proxied server data stream.
func (i *Interposer) Inject(data []byte) {
	_ = i.emulator.Perform(string(data))
}

func (i *Interposer) pullFromUpstream() {
	upstreamBuffer := make([]byte, 4096)
	for {
		n, err := i.upstream.Read(upstreamBuffer)

		if n > 0 {
			// act upon the emulator with the upstream data
			i.emulatorMutex.Lock()
			terminalToHost := []byte(i.emulator.Perform(string(upstreamBuffer[:n])))
			i.pendingRemoteState = terminal.CopyFramebuffer(i.emulator.GetFramebuffer())
			i.emulatorMutex.Unlock()
			if len(terminalToHost) > 0 {
				// write-back e.g. terminal reports generated by the emulator
				if _, err := i.upstream.Write(terminalToHost); err != nil {
					if i.lastUpstreamErr == nil {
						i.lastUpstreamErr = err
					}
					select { // non-blocking put
					case i.upstreamErr <- err:
					default:
						i.droppedUpdate = true
					}
					return
				}
			}

			if !i.pendingEpoch {
				i.completeRemoteState = terminal.CopyFramebuffer(i.pendingRemoteState)
			}
		}

		if i.lastUpstreamErr == nil && err != nil {
			i.lastUpstreamErr = err
		}
		select { // non-blocking put
		case i.upstreamErr <- err:
		default:
			i.droppedUpdate = true
		}
		if err != nil {
			return
		}
	}
}

// Close the terminal.
func (i *Interposer) Close() error {
	if i.opened {
		closeStr := []byte(i.display.Close())
		i.bufferMutex.Lock()
		if i.pending == nil {
			i.pending = &bytes.Buffer{}
		}
		_, _ = io.Copy(i.pending, bytes.NewReader(closeStr))
		i.bufferMutex.Unlock()
	}
	defer func() { _ = i.upstream.Close() }() // close the underlying reader if the asynk fails to, for some reason
	return i.upstreamAsynk.Close()            // close the asynk attached to upstream
}

// Read printed output from the terminal.
func (i *Interposer) Read(p []byte) (int, error) {
	if i.pending != nil {
		i.bufferMutex.Lock()
		defer i.bufferMutex.Unlock()
		// have pending bytes from a previous write to complete
		if n, err := i.pending.Read(p); err == io.EOF {
			i.pending = nil
			return n, nil
		} else {
			return n, err
		}
	}
	if !i.opened {
		// need to send Terminal::Display.open() output first
		i.opened = true
		open := []byte(i.display.Open())
		n := copy(p, open)
		if n < len(open) {
			open = open[n:]
			i.bufferMutex.Lock()
			if i.pending == nil {
				i.pending = &bytes.Buffer{}
			}
			_, _ = io.Copy(i.pending, bytes.NewReader(open))
			i.bufferMutex.Unlock()
		}
		return n, nil
	}

	if i.lastUpstreamErr != nil {
		return 0, i.lastUpstreamErr
	}

	now := time.Now()
	lastUpdatedDelta := now.Sub(i.lastUpdated)
	if lastUpdatedDelta < i.coalesceInterval {
		// last display update was more recent than the coalescence interval, so sleep until we hit that interval
		time.Sleep(i.coalesceInterval - lastUpdatedDelta)
	}

	// check if an upstream read is ready -- otherwise wait until one is received
	isPrediction := false
	if !i.droppedUpdate {
		// choose between upstream data, and predicted data -- if either is pending
		select {
		case err := <-i.upstreamErr:
			if err != nil {
				// got an error from upstream...
				n := 0
				if err == io.EOF {
					// on EOF, send terminal close data too
					closeData := []byte(i.display.Close())
					n = copy(p[n:], closeData)
				}
				return n, err
			}
		case <-i.predictionNotification: // predicted data may be available -- apply prediction overlay on current state
			isPrediction = true
		}
	}
	i.droppedUpdate = false

	// emit new output, based on the last completed epoch we've received
	i.emulatorMutex.Lock()
	remoteFramebufferCopy := terminal.CopyFramebuffer(i.completeRemoteState)
	// with predictions applied...
	i.predictor.Cull(remoteFramebufferCopy) // predictor must cull the target framebuffer before application
	i.predictor.Apply(remoteFramebufferCopy)
	emission := []byte(i.display.NewFrame(i.initialized, i.localState, remoteFramebufferCopy))
	i.initialized = true
	i.localState = remoteFramebufferCopy
	i.emulatorMutex.Unlock()

	n := copy(p, emission)
	if n < len(emission) {
		emission = emission[n:]
		i.bufferMutex.Lock()
		if i.pending == nil {
			i.pending = &bytes.Buffer{}
		}
		_, _ = io.Copy(i.pending, bytes.NewReader(emission))
		i.bufferMutex.Unlock()
	}
	if !isPrediction {
		i.lastUpdated = now
	}

	return n, nil
}

// Write user input to the terminal.
func (i *Interposer) Write(p []byte) (int, error) {
	terminalToHost := &bytes.Buffer{}
	i.emulatorMutex.Lock()

	now := time.Now()
	if i.pendingEpochStarted.IsZero() {
		// start tracking the start of a new un-acknowledged epoch
		i.pendingEpochStarted = now
	} else {
		// tie SetSendInterval to the oldest un-acknowledged epoch -> triggers underlines when server response is slow
		latency := now.Sub(i.pendingEpochStarted)
		i.predictor.SetSendInterval(latency)
	}

	for _, b := range p {
		// write new user bytes to predictor (and the selected framebuffer)
		i.predictor.NewUserByte(b, i.localState)
		s := i.emulator.Act(parser.MakeUserByte(int(b)))
		terminalToHost.WriteString(s)
		if b == 0x0c { // repaint
			i.initialized = false
		}
	}
	if len(p) > 0 {
		// notify that a prediction might be available in response to this user input (non-blocking channel put)
		select {
		case i.predictionNotification <- true:
		default:
			i.droppedUpdate = true
		}
	}

	// increment the epoch to track when we have a response from the server that reflects this input
	i.epoch += 1
	openedEpoch := i.epoch
	i.pendingEpoch = true
	i.predictor.LocalFrameSent(openedEpoch)
	i.emulatorMutex.Unlock()

	// only open the epoch after the payload has been fully transmitted to the host
	n, err := i.upstreamAsynk.WriteNotify(terminalToHost.Bytes(), func() {
		go i.openEpoch(i, openedEpoch, now)
	})
	return n, err
}

// Resize the width and height of the interposed terminal, in response to e.g. SIGWINCH or equivalent signal.
func (i *Interposer) Resize(w, h int) {
	i.emulatorMutex.Lock()
	defer i.emulatorMutex.Unlock()
	i.emulator.Act(parser.MakeResize(int64(w), int64(h)))
	i.width, i.height = w, h
	i.predictor.Reset()
}

// CurrentContents produces a "patch" that transforms a fresh/reset terminal to one that matches the current display
// contents of the interposed terminal. By default, this will show predictions in flight, but this can be disabled by
// the parameter.
func (i *Interposer) CurrentContents(noPrediction bool) string {
	i.emulatorMutex.Lock()
	fb := i.emulator.GetFramebuffer()
	if !noPrediction {
		// copy it so we can apply predictor changes
		fb = terminal.CopyFramebuffer(fb)
	}
	i.emulatorMutex.Unlock()

	if !noPrediction {
		i.predictor.Cull(fb)
		i.predictor.Apply(fb)
	}
	blank := terminal.MakeFramebuffer(i.width, i.height)

	initSize := fmt.Sprintf("\033[8;%d;%dt", i.height, i.width)
	return initSize + i.display.NewFrame(false, blank, fb)
}
