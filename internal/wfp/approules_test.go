package wfp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestAppRulesBlockAndRelease needs an elevated shell and network access. With the tunnel
// "down" (LUID 0) the program must lose network access, and regain it after Close.
func TestAppRulesBlockAndRelease(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("run from an elevated shell")
	}
	curl := filepath.Join(os.Getenv("SystemRoot"), "System32", "curl.exe")
	probe := func() error {
		return exec.Command(curl, "-s", "-o", "NUL", "--max-time", "8", "https://1.1.1.1").Run()
	}
	if err := probe(); err != nil {
		t.Skipf("no network before the test: %v", err)
	}
	var r AppRules
	if err := r.Apply([]string{curl}, 0); err != nil {
		t.Fatal(err)
	}
	if err := probe(); err == nil {
		r.Close()
		t.Fatal("curl reached the network while blocked")
	}
	r.Close()
	if err := probe(); err != nil {
		t.Fatalf("curl still blocked after Close: %v", err)
	}
}

func TestApplyRejectsMissingProgram(t *testing.T) {
	var r AppRules
	if err := r.Apply([]string{`C:\nope\missing.exe`}, 0); err == nil {
		t.Fatal("missing program accepted")
	}
}
