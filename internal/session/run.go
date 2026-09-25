package session

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"runtime"
)

// closeMayStrandReader reports whether a failed Terminal.Close can leave the
// reader blocked. It is a variable so tests can exercise both answers.
var closeMayStrandReader = runtime.GOOS == "windows"

// Run routes packets to the session's services until the session ends, then
// closes opts.Terminal (if set) and waits for every goroutine it started,
// except the terminal reader when a failed Close may have left it blocked. It
// returns nil when the remote shell ended without an exit status, *ExitError
// when it reported one, and ErrDetached when the user typed the local escape.
func Run(ctx context.Context, t Transport, opts Options) error {
	log := cmp.Or(opts.Logger, slog.New(slog.DiscardHandler))

	var services []service
	var term *terminalService
	if opts.Terminal != nil {
		term = newTerminalService(opts.Terminal, log)
		services = append(services, term)
	}
	r, err := newRouter(services, log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	g := &group{cancel: cancel}
	g.Go(func() error { return r.run(ctx, t) })
	for _, s := range services {
		s.start(ctx, t, r.packets(ctx, s), g)
	}

	<-ctx.Done()
	// Closing the terminal is the only way to unblock a pending Read, and the
	// reader goroutine must be joined before Run returns (spec 4.3).
	// console.Close contract (phase 4): on Windows a non-nil error can mean
	// the reader is still blocked in ReadConsoleW (the wake record was taken
	// by another process sharing the console, or Close waited on a stalled
	// Write). Spec 5.6: Run then stops waiting for the reader instead of
	// joining it, and the error is logged at Warn so a hang is visible.
	readerStuck := false
	if opts.Terminal != nil {
		if err := opts.Terminal.Close(); err != nil {
			log.Warn("session: close terminal", "err", err)
			readerStuck = closeMayStrandReader
		}
	}
	g.wg.Wait()
	if term != nil {
		if readerStuck {
			log.Warn("session: not waiting for the terminal reader after a failed close")
		} else {
			term.reader.Wait()
		}
	}

	cause := context.Cause(ctx)
	if errors.Is(cause, errEnded) {
		return nil
	}
	return cause
}
