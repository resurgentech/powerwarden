package main

import (
	"os"
	"strings"
	"syscall"
)

const syscallSIGTERM = syscall.SIGTERM

func shortHostForTest() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return h
}
