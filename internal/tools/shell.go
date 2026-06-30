package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Shell ejecuta una sola línea de comando permitida dentro de Root, con un
// timeout estricto. Es deliberadamente simple: esta es una barrera de
// protección de prueba de concepto, no un sandbox real, así que solo se
// asume uso local de confianza.
type Shell struct {
	Root      string
	Allowlist map[string]bool
	Timeout   time.Duration
}

func NewShell(root string, allowedCommands []string, timeout time.Duration) *Shell {
	allow := make(map[string]bool, len(allowedCommands))
	for _, c := range allowedCommands {
		allow[strings.ToLower(strings.TrimSpace(c))] = true
	}
	return &Shell{Root: root, Allowlist: allow, Timeout: timeout}
}

type ShellResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (s *Shell) Run(commandLine string) (ShellResult, error) {
	fields := strings.Fields(commandLine)
	if len(fields) == 0 {
		return ShellResult{}, fmt.Errorf("empty command")
	}
	program := strings.ToLower(fields[0])
	if len(s.Allowlist) > 0 && !s.Allowlist[program] {
		return ShellResult{}, fmt.Errorf("command %q is not in the shell allowlist", fields[0])
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", commandLine)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", commandLine)
	}
	cmd.Dir = s.Root

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
		err = nil
	} else if ctx.Err() == context.DeadlineExceeded {
		return ShellResult{}, fmt.Errorf("command timed out after %s", s.Timeout)
	}

	return ShellResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}, err
}
