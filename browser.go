package main

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
)

// startCmd launches a program detached, without a shell.
//
// NOTE: on Windows we must NOT route through `cmd /c start`, because the
// shell splits the login URL on `&` (e.g. `?platform=...&state=...` would
// lose the state param). exec.Command(...).Start() detaches fine on its own.
func startCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}

func hasStoredAuth(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := string(raw)
	return strings.Contains(s, "accessToken") && strings.Contains(s, "refreshToken")
}

func bufioWait() {
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}
