//go:build !unix

package bash

import (
	"errors"
	"os/exec"
)

func platformSupported() error {
	return errors.New("bash: built-in tool requires a Unix process environment")
}

func configureProcessGroup(*exec.Cmd) {}
