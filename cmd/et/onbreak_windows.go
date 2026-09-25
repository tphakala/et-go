package main

import "github.com/tphakala/et-go/internal/console"

// onBreak calls f on Ctrl+Break, which Windows raises even in raw mode.
func onBreak(f func()) { console.OnBreak(f) }
