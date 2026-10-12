// Package toolsocket serves a command on per-workspace Unix sockets:
// each connection starts one process of the command and bridges its
// standard input and output to the connection. Sortie uses it to serve
// its MCP tool server, which runs on the host, to an agent running in an
// execution environment that reaches the host only through the socket.
package toolsocket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// Default timings.
const (
	// defaultCheckEvery is how often a listener checks that its socket
	// file is still the one it bound.
	defaultCheckEvery = 2 * time.Second

	// exitGrace is how long a process may keep running after its client
	// ended its input before it is killed.
	exitGrace = 5 * time.Second

	// stderrDrainBound bounds the wait for a reaped process's standard
	// error to reach its end.
	stderrDrainBound = time.Second

	stderrTailBytes = 4096
)

// Spec is the command a socket starts for each connection.
type Spec struct {
	// Argv is the command and its arguments; Argv[0] is an absolute
	// host path.
	Argv []string

	// Env holds the process's KEY=VALUE environment, used verbatim.
	Env []string
}

// Registry holds one listener per workspace key. It is safe for
// concurrent use.
type Registry struct {
	logger     *slog.Logger
	checkEvery time.Duration
	exitGrace  time.Duration

	mu        sync.Mutex
	listeners map[string]*listener
	closed    bool
}

// NewRegistry returns an empty registry that logs to logger.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{logger: logger, checkEvery: defaultCheckEvery, exitGrace: exitGrace, listeners: map[string]*listener{}}
}

// Serve makes key's listener serve spec on the socket at path. A
// listener that already serves path keeps its socket and uses spec for
// every later connection; connections already open keep their process.
// Otherwise a new listener binds path, replacing a socket file left at
// it; a path that holds anything but a socket is an error. The socket
// file is created with mode 0600. A listener whose socket file is
// removed, or replaced by another, stops within a few seconds, so
// removing the socket's directory with its workspace ends the listener
// too.
func (r *Registry) Serve(key, path string, spec Spec) error {
	if len(spec.Argv) == 0 || !filepath.IsAbs(spec.Argv[0]) {
		return fmt.Errorf("tool socket %q: the command must be an absolute path", path)
	}
	spec = Spec{Argv: slices.Clone(spec.Argv), Env: slices.Clone(spec.Env)}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("tool socket registry is closed")
	}
	if l := r.listeners[key]; l != nil {
		if l.path == path && l.intact() {
			l.setSpec(spec)
			return nil
		}
		delete(r.listeners, key)
		l.stop()
	}
	l, err := listen(path, spec, r.exitGrace, r.logger.With(slog.String("tool_socket", path)))
	if err != nil {
		return err
	}
	r.listeners[key] = l
	go l.serve()
	go r.watch(key, l)
	return nil
}

// Stop stops key's listener, closes its connections, kills and reaps
// their processes, and removes its socket file. A key without a listener
// is a no-op.
func (r *Registry) Stop(key string) {
	r.mu.Lock()
	l := r.listeners[key]
	delete(r.listeners, key)
	r.mu.Unlock()
	if l != nil {
		l.stop()
	}
}

// Close stops every listener, as [Registry.Stop] does, and makes later
// calls to [Registry.Serve] fail.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	all := r.listeners
	r.listeners = map[string]*listener{}
	r.mu.Unlock()
	for _, l := range all {
		l.stop()
	}
}

// Serving reports whether key has a listener.
func (r *Registry) Serving(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.listeners[key]
	return ok
}

// watch stops l once its socket file is gone or replaced.
func (r *Registry) watch(key string, l *listener) {
	ticker := time.NewTicker(r.checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
		}
		if l.intact() {
			continue
		}
		r.mu.Lock()
		if r.listeners[key] == l {
			delete(r.listeners, key)
		}
		r.mu.Unlock()
		l.logger.Info("tool socket removed, stopping its listener")
		l.stop()
		return
	}
}

// listener serves one socket.
type listener struct {
	path   string
	ln     *net.UnixListener
	bound  os.FileInfo
	grace  time.Duration
	logger *slog.Logger

	mu    sync.Mutex
	spec  Spec
	conns map[net.Conn]struct{}

	stopOnce sync.Once
	done     chan struct{}
	wg       sync.WaitGroup
}

func listen(path string, spec Spec, grace time.Duration, logger *slog.Logger) (*listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("tool socket %q is not an absolute path", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create tool socket directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("tool socket %q exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale tool socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect tool socket: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on tool socket: %w", err)
	}
	// The file is removed only while it is still this listener's own.
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restrict tool socket: %w", err)
	}
	bound, err := os.Lstat(path)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("inspect tool socket: %w", err)
	}
	return &listener{
		path: path, ln: ln, bound: bound, grace: grace, logger: logger,
		spec: spec, conns: map[net.Conn]struct{}{}, done: make(chan struct{}),
	}, nil
}

// intact reports whether the socket file is still the one l bound.
func (l *listener) intact() bool {
	info, err := os.Lstat(l.path)
	return err == nil && os.SameFile(info, l.bound)
}

func (l *listener) setSpec(spec Spec) {
	l.mu.Lock()
	l.spec = spec
	l.mu.Unlock()
}

// stop closes the listener and every connection, waits for their
// processes, and removes the socket file while it is still l's own.
func (l *listener) stop() {
	l.stopOnce.Do(func() {
		close(l.done)
		_ = l.ln.Close()
		if l.intact() {
			_ = os.Remove(l.path)
		}
		l.mu.Lock()
		for c := range l.conns {
			_ = c.Close()
		}
		l.mu.Unlock()
	})
	l.wg.Wait()
}

func (l *listener) serve() {
	for {
		conn, err := l.ln.AcceptUnix()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			l.logger.Warn("tool socket accept failed", slog.Any("error", err))
			time.Sleep(100 * time.Millisecond)
			continue
		}
		l.mu.Lock()
		select {
		case <-l.done:
			l.mu.Unlock()
			_ = conn.Close()
			return
		default:
		}
		spec := l.spec
		l.conns[conn] = struct{}{}
		l.wg.Add(1)
		l.mu.Unlock()
		go func() {
			defer l.wg.Done()
			l.handle(conn, spec)
			l.mu.Lock()
			delete(l.conns, conn)
			l.mu.Unlock()
		}()
	}
}

// handle runs one process of spec for conn. The process ends with the
// connection: when the client ends its input, the process's input ends
// and it is killed if it is still running after a grace period; when
// the connection fails, it is killed at once. The connection closes once
// the process has exited and its output is relayed.
func (l *listener) handle(conn *net.UnixConn, spec Spec) {
	defer conn.Close() //nolint:errcheck // the connection is done either way

	ctx, kill := context.WithCancel(context.Background())
	defer kill()
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // G204: Sortie's own tool server command
	procutil.SetGroupKill(cmd)
	cmd.Env = spec.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		l.logger.Warn("tool server start failed", slog.Any("error", err))
		return
	}
	pipes, _, err := procutil.StartWithOwnedPipes(ctx, cmd, l.logger)
	if err != nil {
		_ = stdin.Close()
		l.logger.Warn("tool server start failed", slog.Any("error", err))
		return
	}
	defer pipes.Close() //nolint:errcheck // both read ends are done once the reap ends
	reaper := procutil.StartReaper(cmd, l.logger)

	stderr := &tailBuffer{max: stderrTailBytes}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderr, pipes.Stderr)
	}()

	go func() {
		_, copyErr := io.Copy(stdin, conn)
		_ = stdin.Close()
		if copyErr != nil {
			kill()
			return
		}
		select {
		case <-reaper.Done():
		case <-time.After(l.grace):
			kill()
		}
	}()

	if _, err := io.Copy(conn, pipes.Stdout); err != nil {
		kill()
	}
	<-reaper.Done()
	select {
	case <-stderrDone:
	case <-time.After(stderrDrainBound):
	}
	if waitErr := reaper.Err(); waitErr != nil {
		l.logger.Debug("tool server exited", slog.Any("error", waitErr), slog.String("stderr_tail", stderr.String()))
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = slices.Clone(t.buf[over:])
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
