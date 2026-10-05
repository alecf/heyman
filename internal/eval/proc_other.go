//go:build !unix

package eval

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}
