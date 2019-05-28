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

	pending *bytes.Buffer

	width, height int

	controlMutex *sync.Mutex

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

func Interpose(rwc io.ReadWriteCloser, coalesceInterval time.Duration, width, height int) *Interposer {
	return &Interposer{
		upstream:         rwc,
		coalesceInterval: coalesceInterval,

		pending: nil,

		width:  width,
		height: height,

		controlMutex: &sync.Mutex{},

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
		if i.pending == nil {
			i.pending = &bytes.Buffer{}
		}
		_, _ = io.Copy(i.pending, bytes.NewReader(closeStr))
	}
	return i.upstream.Close()
}

// Read printed output from the terminal.
func (i *Interposer) Read(p []byte) (int, error) {
	if i.pending != nil {
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
			if i.pending == nil {
				i.pending = &bytes.Buffer{}
			}
			_, _ = io.Copy(i.pending, bytes.NewReader(open))
		}
		return n, nil
	}
	// TODO act upon the output and write-back terminal reports generated by the emulator
	// TODO check if coalescence is ready to emit new output
	return i.upstream.Read(p) // TODO work in progress
}

// Write user input to the terminal.
func (i *Interposer) Write(p []byte) (int, error) {
	terminalToHost := &bytes.Buffer{}
	for _, b := range p {
		s := i.emulator.Act(parser.MakeUserByte(int(b)))
		terminalToHost.WriteString(s)
	}
	return i.upstream.Write(terminalToHost.Bytes())
}

// Change the width and height of the interposed terminal, in response to e.g. SIGWINCH or equivalent signal.
func (i *Interposer) Resize(w, h int) {
	i.controlMutex.Lock()
	defer i.controlMutex.Unlock()
	i.emulator.Act(parser.MakeResize(int64(w), int64(h)))
}

// Produce a "patch" that transforms a fresh/reset terminal to one that matches the current display contents of the
// interposed terminal.
func (i *Interposer) CurrentContents() string {
	i.controlMutex.Lock()
	defer i.controlMutex.Unlock()
	fb := i.emulator.GetFramebuffer()
	blank := terminal.MakeFramebuffer(i.width, i.height)
	return i.display.NewFrame(false, blank, fb)
}
