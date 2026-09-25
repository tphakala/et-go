// Command et is a native Eternal Terminal client: it starts etterminal on
// the server over ssh, then runs an encrypted, reconnecting session with
// etserver.
package main

import (
	"context"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
