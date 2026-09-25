// Command et is a native Eternal Terminal client.
//
// This is a scaffold placeholder: it only reports its version. The client is
// specified in the local design spec and built out by the implementation plan.
package main

import (
	"fmt"
	"os"
)

// version is overridden at release time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "et %s: not implemented yet\n", version)
	os.Exit(1)
}
