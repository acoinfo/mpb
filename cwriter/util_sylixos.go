//go:build sylixos

package cwriter

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TCGETS

