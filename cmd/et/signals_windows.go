package main

import "os"

// shutdownSignals end the client. In raw mode Ctrl+C is a byte sent to the
// remote. Ctrl+Break arrives here as os.Interrupt until the session has
// started; from then on console.OnBreak turns it into a detach.
var shutdownSignals = []os.Signal{os.Interrupt}
