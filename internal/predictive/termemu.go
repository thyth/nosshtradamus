package predictive

import (
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh"
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh/overlay"
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh/parser"
	"gitlab.hive.thyth.com/chronostruct/go-mosh/pkg/mosh/terminal"

	"bytes"
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
type Interposer struct {
	upstream        io.ReadWriteCloser
	upstreamAsynk   io.WriteCloser
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

	pendingEpoch           bool                      // is an update pending (hack)
	predictor              *overlay.PredictionEngine // speculative/predictive engine
	predictionNotification chan interface{}

	opened, initialized bool
}

type InterposerOptions struct {
	CoalesceInterval         time.Duration
	DisplayPreference        overlay.DisplayPreference
	DisplayPredictOverwrites bool

	PreFilter func(io.ReadWriteCloser, *Interposer) io.ReadWriteCloser
}

// GetDefaultInterposerOptions produces a set of reasonable defaults for the interposer's prediction and coalescing
// parameters. Customize as needed in consumers of the interposer.
func GetDefaultInterposerOptions() *InterposerOptions {
	return &InterposerOptions{
		// Specifies the time interval within which multiple updates to the terminal are coalesced into a single delta
		// by Mosh. Default is 60 frames per second.
		CoalesceInterval: time.Second / 60,

		// Specifies the default prediction mode. Using "experimental", as it is the most aggressive.
		DisplayPreference: overlay.PredictExperimental,
		// TODO note: setting this to PredictAlways seems to show more correct cursor behavior than experimental - why?

		// Specifies if the prediction should include character overwrite predictions. Enabling for greater aggression.
		DisplayPredictOverwrites: true,
	}
}

// Notes:
// - The 'termemu.cc' example program utilizes the Mosh terminal emulator classes to coalesce multiple updates into 20
//   millisecond deltas. It instantiates instances of Terminal::Complete, Terminal::Framebuffer, Terminal::Display,
//   Parser::UserByte, and Parser::Resize.
//   - There are only 3 functions: main, emulate_terminal, and tick.
//   - main probes terminal capabilities, launches a child process shell, and invokes emulate_terminal in the parent.
//     - Informs the child shell that the TERM is 'xterm-256color', and NCURSES_NO_UTF8_ACS=1 via environment.
//     - The latter environment variable informs ncurses programs to use different (mosh compatible?) line drawing mode.
//   - emulate_terminal retrieves the current terminal size (of STDIN), informs the child of the terminal size (via its
//     fd), allocates a Terminal::Complete and Terminal::Framebuffer using that terminal size, allocates a
//     Terminal::Display, outputs the display.open() string to STDOUT, and then begins a 3 clause select.
//     - The first clause reads user input from STDIN, bytewise wraps the characters of that input with Parser::UserByte
//       instances, feeds those to the Terminal::Complete instance via complete.act(...), then writes the accumulated
//       string ("terminal_to_host") to the child process (so that the child can react to user input).
//     - The second clause reads child output (via its fd) as a string, and feeds it as one chunk to complete.act(...)
//       similar to how the Parser::Action (e.g. Parser::UserByte) instances are used. This ALSO generates a string
//       "terminal_to_host" that is fed back to the child process(!). No output is sent to STDOUT in this clause!
//       - This is presumably to support terminal control sequences that report state (e.g. mouse position?) back to the
//         child process, and is probably empty a lot of the time.
//     - The third clause is triggered by SIGWINCH signals. These designate size changes of the parent terminal. The
//       Terminal::Complete instance is first notified via complete.act(Parser::Resize(cols, rows)), and then the same
//       size change is forwarded to the child process.
//     - This select runs subject to an optional timeout. Initially, this waits indefinitely for one of these 3 clauses
//       to be triggered. If a "tick" is valid (i.e. a new terminal has been drawn), a timeout is set for 20 ms, and
//       subsequent triggers of the clauses will be coalesced until that timeout expires (forcing another call to tick).
//     - After one of the clauses runs, or the timeout is triggered, the Terminal::Complete instance is asked for the
//       current framebuffer state with complete.get_fb() -- as there is no prior linkage between the previously created
//       Terminal::Framebuffer instance, this new framebuffer must be freshly allocated?
//     - At this point, tick() is called with the existing state Terminal::Framebuffer, the new framebuffer just created
//       by calling complete.get_fb(), and the Terminal::Display instance. The 3 clause select loop then iterates again.
//     - Once the loop is broken, a final call to display.new_frame(...) is called to capture any unsent update, with
//       the output string sent to STDOUT, and then the string produced by display.close() is also sent to STDOUT as a
//       terminal cleanup method.
//   - tick prints a frame delta to STDOUT and sets the (old) state reference to the new_frame, provided the last frame
//     was drawn at least 20 milliseconds ago. It also keeps track of whether the display has been initialized (which
//     is fed into the delta update creation function), and sets this initialization to (statically) true after sending
//     the first update. These updates wre written to STDOUT, and represents the only way that output fed from the child
//     process can reach the parent terminal (via state accumulated within the Terminal::Complete emulator, and the
//     framebuffer instances returned from it).
//
// - The 'benchmark.cc' example program uses the Overlay::OverlayManager along with a similar setup of a
//   Terminal::Complete, (pair of) Terminal::Framebuffer, and Terminal::Display to benchmark the performance of a
//   prediction application of a "user byte" and the generation of Display diffs -- the core operations.
//   - Unlike the 'termemu.cc' example, this is not hooked up to be a faithful terminal emulator.
//   - Instead of wrapping the Overlay::OverlayManager, in go-mosh, I wrapped the underlying Overlay::PredictionEngine.
//     The OverlyManager itself wraps 3 independent pieces of functionality: title injection, a message overlay bar, and
//     the prediction engine. When .apply(...) is called on the OverlayManager (like in 'benchmark.cc'), it effectively
//     calls a .apply(...) function on each of those independent pieces. Since the other two classes are not necessary
//     for the predictive terminal emulator task, they are not wrapped by go-mosh.
//   - At each iteration, a "random" user byte is applied to the prediction engine via .new_user_byte(...). This call
//     takes both the byte being applied (i.e. the effect of a user keystroke in normal operation), *and* it also takes
//     a reference to the "local_framebuffer" Terminal::Framebuffer instance.
//     - Presumably this operation mutates the state of that Framebuffer.
//   - After "typing" a character, the current framebuffer is gathered from the terminal emulator instance. Note that
//     there are no direct calls to .apply(<action>) or .apply(<string>) [called .Perform(...) in go-mosh] on the
//     emulator.
//     - This framebuffer is reference stored in new_state, one of the two framebuffer slots created by 'benchmark.cc'.
//   - Next, the overlay is applied to the framebuffer state just retrieved from the terminal emulator.
//     - Mechanically, this .apply(...) call should be doable directly to the prediction engine rather than the overlay
//       manager, and the correct functionality should be invoked.
//     - Presumably this operation mutates the framebuffer state retrieved from the terminal emulator.
//       - This seems to be the only linkage in this example program between a Terminal::Complete instance and an
//         Overlay::PredictionEngine.
//   - Finally, a delta is computed between the "front" and "back" framebuffers, and then this delta is discarded.
// - Unfortunately, since the 'benchmark.cc' program is so stripped down, it doesn't look like a great example for
//   learning and understanding the flow of data in the context of a more realistic terminal emulator scenario. May need
//   to look at how this is used in other parts of the Mosh code before it will be clear.
//
// - As an alternative to looking at 'benchmark.cc', the Mosh client implementation in 'stmclient.h' and 'stmclient.cc'
//   is where this logic is actually used in the full system.
//   - It has network transport, connection, and data stream logic that is not relevant to this project, but also all
//     of the pieces composed in the 'benchmark.cc' example.
//   - There is a pair of Terminal::Framebuffer instances (local_framebuffer, new_state), a Terminal::Display, and an
//     Overlay::OverlayManager. It also tracks termios data and window dimensions, and a few variables for escape
//     sequence state transitions.
//   - The (constructor) initial framebuffers are created as 1x1 width/height.
//   - In addition to the prediction engine display preference setting (always/never/adaptive/experimental), there is a
//     separate boolean setting controlling whether the prediction engine should predict overwrites. Presumably the most
//     aggressive prediction setting is experimental display preference with overwrite prediction enabled.
//   - A comment indicates that Terminal::Display initialization with boolean true looks at the TERM environment
//     variable for capability and correct terminal initialization data.
//   - There are 5 private member functions on STMClient of strong interest:
//     - void main_init(void):
//       - Called by public .main() member function.
//       - Initializes signal handlers and auxiliary structures.
//       - Determines the size of the host terminal, allocates a new local_framebuffer of those dimensions.
//       - Allocates a 1x1 framebuffer for new_state -- presumably discarded before used.
//       - Initializes the host terminal by calling display.new_frame(false, local_framebuffer, local_framebuffer).
//       - Tells the remote server of the size of the host terminal. Presumably this translates into a call to apply a
//         Parser::Resize instance on a Terminal::Complete instance on the remote end.
//       - Returns to STMClient::Main, which starts a loop-select over:
//         - Receiving data from the network, calling process_network_input()
//         - Reading from the host terminal via the STDIN fd, calling process_user_input(STDIN), and if a false value is
//           returned, it checks to see if the network connection is lost or intentionally closed.
//         - Catching SIGWINCH for host terminal resizing, calling process_resize(), and if a false value is returned,
//           it aborts the loop.
//         - Several other cases covering SIGTERM/SIGINT/SIGHUP/SIGPIPE, SIGCONT, and a variety of networking/crypto
//           failure/reconnection cases.
//     - void process_network_input(void):
//       - Calls network->recv() -- likely effectively equivalent to calling terminal.Complete.Perform(<data>), per the
//         'termemu.cc' example already dissected before this.
//       - Provides timestamp and send interval data to the prediction engine by calls to set_local_frame_acked,
//         set_send_interval, and set_local_frame_late_acked. May need to experiment here?
//       - Does not appear to be a direct connection between the overlay/prediction engine and data in this function.
//     - bool process_user_input(int fd):
//       - Reads up to 16K at a time from STDIN.
//       - Notifies the prediction engine with set_local_frame_sent(net.get_sent_state_last()). Needs experiment here?
//       - If more than 100 bytes are read, it's considered a "paste" operation, and the prediction engine is reset.
//         - Presumably for speed/expediency reasons? Probably will disable this logic in this implementation.
//       - In a loop, each byte is consumed in two places:
//         - A call to .new_user_byte(<byte>, local_framebuffer) -- unless "pasting"
//         - Creation and transmission of a new Parser::UserByte over the network to the remote terminal. This should
//           be equivalent to calling .act(...) with that Parser::UserByte on a Terminal::Complete instance.
//       - A bunch of logic runs for Mosh's local escape sequence handling, which is irrelevant here.
//     - bool process_resize(void):
//       - Gets the current terminal dimensions and sends a Parser::Resize event over the network connection (similar to
//         the behavior of sending size in main_init).
//       - Suggests that the remote end will probably reply with its own resize event so that local state gets updated.
//       - Calls .reset() on the prediction engine. Apparently the effects of a resize are not predicable by Mosh.
//       - Only returns false if it fails to retrieve the host terminal dimensions (which should never happen here).
//     - void output_new_frame(void):
//       - Retrieves the latest remote state from the network object. Likely equivalent to calling .get_fb() on a
//         Terminal::Complete instance. Assigns it to 'new_state' instance Terminal::Framebuffer reference.
//       - Invokes overlay.apply(new_state) to apply the effects of the prediction engine (and other overlays) to that
//         framebuffer.
//       - Calculates a delta terminal update string between local_framebuffer and this new_state including the locally
//         applied overlays. The first init flag can be set to false if a redraw has been requested. Presumably this
//         sends terminal reset codes and draws the terminal from scratch. The redraw request flag is then cleared.
//       - The delta update string is written to STDOUT (the host terminal), and local_framebuffer is overwritten with
//         new_state. Presumably this requires a clone call to terminal.CopyFramebuffer(...) in Golang.
//   - The purpose of Terminal::Display.open() is described as "Put terminal in application-cursor-key mode".
//   - The purpose of Terminal::Display.close() is described as "Restore terminal and terminal-driver state".

func Interpose(rwc io.ReadWriteCloser, options *InterposerOptions) *Interposer {
	inter := &Interposer{
		upstreamErr: make(chan error),

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

		pendingEpoch:           false,
		predictor:              overlay.MakePredictionEngine(),
		predictionNotification: make(chan interface{}),

		opened:      false,
		initialized: false,
	}
	inter.predictor.SetDisplayPreference(options.DisplayPreference)
	inter.predictor.SetPredictOverwrite(options.DisplayPredictOverwrites)

	if options.PreFilter != nil {
		rwc = options.PreFilter(rwc, inter)
	}
	inter.upstream = rwc
	inter.upstreamAsynk = MakeAsynk(inter.upstream, 8192) // TODO make this flow through prefilter?

	go inter.pullFromUpstream()
	return inter
}

func (i *Interposer) SpeculateEpoch(epoch uint64) {
	i.pendingEpoch = true
	i.predictor.LocalFrameSent(epoch)
}

func (i *Interposer) CompleteEpoch(epoch uint64, pending bool) {
	i.emulatorMutex.Lock()
	i.predictor.LocalFrameAcked(epoch)
	i.predictor.LocalFrameLateAcked(epoch)
	//i.predictor.SetSendInterval(100 * time.Millisecond) // TODO defaults to 250 ms in the mosh code?
	// Note: Not invoking i.predictor.SetSendInterval(<duration>) like Mosh does.

	// TODO when complete epoch matches the current speculative epoch, also need to copy pending -> complete (since nothing is pending)
	// TODO ... otherwise no terminal outputs will occur that are not in response to a terminal input!!!
	i.completeRemoteState = terminal.CopyFramebuffer(i.pendingRemoteState)
	i.pendingEpoch = pending
	i.emulatorMutex.Unlock()

	// notify update
	select {
	case i.upstreamErr <- nil:
	default:
		i.droppedUpdate = true
	}
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

			// FIXME hack
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

	// TODO this almost works... but is still not *quite* right
	// emit new output
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
	i.emulatorMutex.Unlock()
	return i.upstreamAsynk.Write(terminalToHost.Bytes())
}

// Change the width and height of the interposed terminal, in response to e.g. SIGWINCH or equivalent signal.
func (i *Interposer) Resize(w, h int) {
	i.emulatorMutex.Lock()
	defer i.emulatorMutex.Unlock()
	i.emulator.Act(parser.MakeResize(int64(w), int64(h)))
	i.predictor.Reset()
}

// Produce a "patch" that transforms a fresh/reset terminal to one that matches the current display contents of the
// interposed terminal. By default, this will show predictions in flight, but this can be disabled by the parameter.
func (i *Interposer) CurrentContents(noPrediction bool) string {
	i.emulatorMutex.Lock()
	width, height := i.width, i.height
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
	blank := terminal.MakeFramebuffer(width, height)

	return i.display.NewFrame(false, blank, fb)
}
