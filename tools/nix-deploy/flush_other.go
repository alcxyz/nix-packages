//go:build !linux && !darwin

package main

import "os"

// flushInput discards input typed on the terminal and not yet read; on this
// platform it does not, so a stray line can still answer a question.
func flushInput(f *os.File) {}
