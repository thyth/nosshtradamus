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

// DefaultCoalesceInterval specifies the time interval within which multiple updates to the terminal are coalesced into
// a single delta by Mosh. Using 60 frames per second.
const DefaultCoalesceInterval = time.Second / 60

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
	upstream         io.ReadWriteCloser
	coalesceInterval time.Duration

	pending        *bytes.Buffer
	upstreamBuffer []byte

	width, height int

	controlMutex, emulatorMutex *sync.Mutex

	state     *terminal.Framebuffer     // current state of the client's terminal
	display   *terminal.Display         // used to generate deltas between framebuffers
	emulator  *terminal.Complete        // processor of terminal control sequences
	predictor *overlay.PredictionEngine // speculative/predictive engine

	opened, initialized bool
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
//   - TODO This method of retrieving Terminal::Framebuffer instances from Terminal::Complete suggests callee memory
//     TODO ownership. Need to double check to see if the current (v0.1.1) go-mosh implementation leaks memory.
//     - FIXME: Probably not leaking memory, since the emulator's framebuffer seems lifetime bound to the emulator?
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

func Interpose(rwc io.ReadWriteCloser, coalesceInterval time.Duration, width, height int) *Interposer {
	return &Interposer{
		upstream:         rwc,
		coalesceInterval: coalesceInterval,

		pending: nil,

		width:  width,
		height: height,

		controlMutex:  &sync.Mutex{},
		emulatorMutex: &sync.Mutex{},

		state:     terminal.MakeFramebuffer(width, height),
		display:   terminal.MakeDisplay(true),
		emulator:  terminal.MakeComplete(width, height),
		predictor: overlay.MakePredictionEngine(),

		opened:      false,
		initialized: false,
	}
}

// Close the terminal.
func (i *Interposer) Close() error {
	if i.opened {
		closeStr := []byte(i.display.Close())
		i.controlMutex.Lock()
		if i.pending == nil {
			i.pending = &bytes.Buffer{}
		}
		_, _ = io.Copy(i.pending, bytes.NewReader(closeStr))
		i.controlMutex.Unlock()
	}
	return i.upstream.Close()
}

// Read printed output from the terminal.
func (i *Interposer) Read(p []byte) (int, error) {
	if i.pending != nil {
		i.controlMutex.Lock()
		defer i.controlMutex.Unlock()
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
			i.controlMutex.Lock()
			if i.pending == nil {
				i.pending = &bytes.Buffer{}
			}
			_, _ = io.Copy(i.pending, bytes.NewReader(open))
			i.controlMutex.Unlock()
		}
		return n, nil
	}

	// read data from upstream
	if i.upstreamBuffer == nil {
		i.upstreamBuffer = make([]byte, len(p))
	}
	n, err := i.upstream.Read(i.upstreamBuffer)
	if err != nil {
		n = copy(p, i.upstreamBuffer[:n])
		if err == io.EOF {
			// on EOF, send terminal close data too
			closeData := []byte(i.display.Close())
			copy(p[n:], closeData)
		}
		return n, err
	}

	// act upon the emulator with the upstream data
	terminalToHost := []byte(i.emulator.Perform(string(i.upstreamBuffer[:n])))
	if len(terminalToHost) > 0 {
		// write-back e.g. terminal reports generated by the emulator
		if n, err := i.Write(terminalToHost); err != nil {
			return n, err
		}
	}

	// TODO check if coalescence is ready to emit new output
	newFrame := i.emulator.GetFramebuffer()
	emission := []byte(i.display.NewFrame(i.initialized, i.state, newFrame))
	i.initialized = true
	i.state = terminal.CopyFramebuffer(newFrame) // TODO does this leak memory?

	n = copy(p, emission)
	if n < len(emission) {
		emission = emission[n:]
		i.controlMutex.Lock()
		if i.pending == nil {
			i.pending = &bytes.Buffer{}
		}
		_, _ = io.Copy(i.pending, bytes.NewReader(emission))
		i.controlMutex.Unlock()
	}

	return n, nil
}

// Write user input to the terminal.
func (i *Interposer) Write(p []byte) (int, error) {
	terminalToHost := &bytes.Buffer{}
	i.emulatorMutex.Lock()
	for _, b := range p {
		s := i.emulator.Act(parser.MakeUserByte(int(b)))
		terminalToHost.WriteString(s)
	}
	i.emulatorMutex.Unlock()
	return i.upstream.Write(terminalToHost.Bytes())
}

// Change the width and height of the interposed terminal, in response to e.g. SIGWINCH or equivalent signal.
func (i *Interposer) Resize(w, h int) {
	i.emulatorMutex.Lock()
	defer i.emulatorMutex.Unlock()
	i.emulator.Act(parser.MakeResize(int64(w), int64(h)))
}

// Produce a "patch" that transforms a fresh/reset terminal to one that matches the current display contents of the
// interposed terminal.
func (i *Interposer) CurrentContents() string {
	i.emulatorMutex.Lock()
	width, height := i.width, i.height
	fb := i.emulator.GetFramebuffer()
	i.emulatorMutex.Unlock()

	blank := terminal.MakeFramebuffer(width, height)
	return i.display.NewFrame(false, blank, fb)
}
