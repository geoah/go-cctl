package cctl

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sshTarget returns user@host (or just host) for a server.
func sshTarget(s Server) string {
	if s.User != "" {
		return s.User + "@" + s.Host
	}
	return s.Host
}

// sshArgs builds the ssh argv (without the target/command).
// Suitable for both direct ssh invocation and the mosh --ssh= string.
func sshArgs(s Server) []string {
	args := []string{}
	if s.SSHKey != "" {
		args = append(args, "-i", expandPath(s.SSHKey))
	}
	if s.Port != 0 {
		args = append(args, "-p", strconv.Itoa(s.Port))
	}
	// Fail fast on an unreachable host instead of the OS TCP default (often
	// 75s+). ConnectTimeout is integer seconds; round up, floor at 1. A
	// user-supplied -o ConnectTimeout in SSHOpts still wins (it comes after).
	secs := int((s.connectTimeout() + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	args = append(args, "-o", "ConnectTimeout="+strconv.Itoa(secs))
	args = append(args, s.SSHOpts...)
	return args
}

// remoteCmd returns an *exec.Cmd that runs cmd via the right transport for
// this server (bash -c locally, ssh otherwise). cmd is a single shell string.
func remoteCmd(s Server, cmd string) *exec.Cmd {
	if s.Local {
		return exec.Command("bash", "-c", cmd)
	}
	args := sshArgs(s)
	args = append(args, sshTarget(s), cmd)
	return exec.Command("ssh", args...)
}

// runRemote runs cmd on the remote (or locally for s.Local) and returns stdout.
// Non-zero exit is an error and includes captured stderr.
func runRemote(s Server, cmd string) (string, error) {
	target := transportLabel(s)
	start := time.Now()
	log().Debug("remote-run", "target", target, "cmd", abbrev(cmd, 200))
	c := remoteCmd(s, cmd)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	dur := time.Since(start)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			tag := "ssh"
			if s.Local {
				tag = "bash"
			}
			// Log the FULL cmd + stderr on failure so ~/.cctl.log has
			// enough to reproduce the failure without re-running. The
			// happy-path log still abbreviates to keep volume sane.
			log().Warn("remote-run-fail",
				"target", target, "dur", dur.String(),
				"exit", ee.ExitCode(),
				"cmd", cmd,
				"stdout", strings.TrimSpace(stdout.String()),
				"stderr", strings.TrimSpace(stderr.String()),
			)
			return stdout.String(), fmt.Errorf("%s exit %d: %s", tag, ee.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		log().Error("remote-run-error", "target", target, "dur", dur.String(), "err", err.Error())
		return stdout.String(), err
	}
	log().Debug("remote-run-ok",
		"target", target, "dur", dur.String(),
		"stdout_bytes", stdout.Len(),
	)
	return stdout.String(), nil
}

// runRemoteCode runs cmd and returns its exit code without treating non-zero as
// an error. Used for `tmux has-session` where non-zero just means "not found".
func runRemoteCode(s Server, cmd string) (int, string, error) {
	target := transportLabel(s)
	start := time.Now()
	log().Debug("remote-code", "target", target, "cmd", abbrev(cmd, 200))
	c := remoteCmd(s, cmd)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	dur := time.Since(start)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			log().Debug("remote-code-nonzero", "target", target, "dur", dur.String(), "exit", ee.ExitCode())
			return ee.ExitCode(), stdout.String() + stderr.String(), nil
		}
		log().Error("remote-code-error", "target", target, "dur", dur.String(), "err", err.Error())
		return -1, "", err
	}
	log().Debug("remote-code-ok", "target", target, "dur", dur.String())
	return 0, stdout.String(), nil
}

// sshOptionValues extracts the bare option values ("IdentitiesOnly=yes")
// from a raw ssh argv-style opts list ("-o", "IdentitiesOnly=yes", …) —
// the form `cmux ssh --ssh-option` wants. Both "-o value" pairs and
// "-ovalue" single tokens are handled; flags that aren't -o options
// (rare in ssh_opts) are skipped with a log line since cmux ssh has no
// generic passthrough for them.
func sshOptionValues(opts []string) []string {
	var out []string
	for i := 0; i < len(opts); i++ {
		switch {
		case opts[i] == "-o" && i+1 < len(opts):
			out = append(out, opts[i+1])
			i++
		case strings.HasPrefix(opts[i], "-o") && len(opts[i]) > 2:
			out = append(out, opts[i][2:])
		default:
			log().Debug("ssh-opt-skipped-for-cmux-ssh", "opt", opts[i])
		}
	}
	return out
}

// transportLabel produces a stable identifier for logs ("local" or
// "user@host"). Avoids spamming logs with full ssh argv.
func transportLabel(s Server) string {
	if s.Local {
		return "local"
	}
	return sshTarget(s)
}

// interactiveCmd builds the exec.Cmd that opens an interactive shell to run cmd
// on s (bash for local, mosh or ssh -t for remote). Stdin/stdout/stderr are not
// wired up — callers do that.
func interactiveCmd(s Server, useMosh bool, cmd string) (*exec.Cmd, error) {
	if s.Local {
		bash, err := exec.LookPath("bash")
		if err != nil {
			return nil, fmt.Errorf("bash not found in PATH: %w", err)
		}
		return exec.Command(bash, "-c", cmd), nil
	}
	if useMosh {
		mosh, err := exec.LookPath("mosh")
		if err != nil {
			return nil, fmt.Errorf("mosh not found in PATH: %w", err)
		}
		ssh := append([]string{"ssh"}, sshArgs(s)...)
		args := []string{"--ssh=" + joinShell(ssh), sshTarget(s), "--", "sh", "-lc", cmd}
		return exec.Command(mosh, args...), nil
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("ssh not found in PATH: %w", err)
	}
	args := []string{"-t"}
	args = append(args, sshArgs(s)...)
	args = append(args, sshTarget(s), cmd)
	return exec.Command(ssh, args...), nil
}

// execInteractive replaces this process with the interactive command from
// interactiveCmd. Never returns on success.
func execInteractive(s Server, useMosh bool, cmd string) error {
	c, err := interactiveCmd(s, useMosh, cmd)
	if err != nil {
		return err
	}
	return syscall.Exec(c.Path, c.Args, os.Environ())
}

// joinShell joins args into a single string with POSIX shell quoting where
// needed. Used to embed an ssh invocation inside mosh's --ssh= flag.
func joinShell(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

// shellQuote wraps s in single quotes if it contains anything outside the safe set.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '_', '-', '.', '/', '=', ':', '@', '+', ',':
			continue
		}
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// shellPath quotes a path for embedding in a shell command. A leading "~" or
// "~/" is rewritten to "$HOME" so the shell will expand it (tildes inside any
// kind of quotes are not expanded). Everything else is double-quoted with
// $/`/"/\ escaped.
func shellPath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		return `"$HOME/` + escapeDoubleQuoted(p[2:]) + `"`
	}
	return `"` + escapeDoubleQuoted(p) + `"`
}

func escapeDoubleQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '"', '\\', '`', '$':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
