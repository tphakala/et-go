package main

import "os"

// shutdownSignals end the client. In raw mode Ctrl+C is a byte sent to the
// remote; Ctrl+Break is handled by console.OnBreak instead.
var shutdownSignals = []os.Signal{os.Interrupt}
