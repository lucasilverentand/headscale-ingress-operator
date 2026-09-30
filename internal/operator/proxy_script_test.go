//go:build unix

package operator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Run the actual wrapper with stand-ins for the two Tailscale binaries. These
// tests exercise shell process handling rather than just matching script text.
func proxyScriptCommand(t *testing.T, boot, status, serve string) (*exec.Cmd, string) {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{
		"containerboot": "#!/bin/sh\n" + boot,
		"tailscale":     "#!/bin/sh\ncase \"$1\" in\nstatus) " + status + ";;\nserve) " + serve + ";;\nesac\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	script := proxyTailscaleScript
	for _, name := range []string{"containerboot", "tailscale"} {
		script = strings.ReplaceAll(script, "/usr/local/bin/"+name, strconv.Quote(filepath.Join(dir, name)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Avoid leaving a mock child behind if an assertion or timeout fails.
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	return cmd, dir
}

func TestProxyScriptPropagatesExit(t *testing.T) {
	for _, tc := range []struct {
		name, boot, status, serve string
		wantExit                  int
		wantServe                 bool
	}{
		{"failed_before_ready", "exit 23", "exit 1", "touch served", 23, false},
		{"clean_exit_before_ready", "exit 0", "exit 1", "touch served", 1, false},
		{"healthy_start", "sleep 1; exit 0", "exit 0", "touch served", 0, true},
		{"failed_after_ready", "sleep 1; exit 23", "exit 0", "touch served", 23, true},
		{"serve_failure", "sleep 1; exit 0", "exit 0", "touch served; exit 7", 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, dir := proxyScriptCommand(t, tc.boot, tc.status, tc.serve)
			err := cmd.Run()
			gotExit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tc.wantExit {
				t.Fatalf("exit = %d, want %d", gotExit, tc.wantExit)
			}
			_, err = os.Stat(filepath.Join(dir, "served"))
			if (err == nil) != tc.wantServe {
				t.Fatalf("serve invoked = %t, want %t", err == nil, tc.wantServe)
			}
		})
	}
}

func TestProxyScriptForwardsTermination(t *testing.T) {
	for _, phase := range []string{"startup", "serving"} {
		t.Run(phase, func(t *testing.T) {
			status := "exit 1"
			if phase == "serving" {
				status = "test -f started"
			}
			cmd, dir := proxyScriptCommand(t,
				"trap 'touch terminated; exit 0' TERM INT\ntouch started\nwhile :; do sleep 0.1; done",
				status, "touch served")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			marker := "started"
			if phase == "serving" {
				marker = "served"
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("wrapper did not reach " + phase)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "terminated")); err != nil {
				t.Fatal("containerboot did not receive termination")
			}
		})
	}
}
