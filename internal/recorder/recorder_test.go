package recorder

import (
	"strings"
	"testing"
)

func TestRedactStripsCredentialsFromRTSPURLs(t *testing.T) {
	line := `[in#0 @ 0x55f58698f440] Error opening input: rtsp://admin:hunter2@192.168.178.45:554/stream1`
	got := redact(line)
	if strings.Contains(got, "hunter2") || strings.Contains(got, "admin:") {
		t.Errorf("redact leaked credentials: %q", got)
	}
	if !strings.Contains(got, "rtsp://***:***@192.168.178.45:554/stream1") {
		t.Errorf("redact produced unexpected output: %q", got)
	}
}

func TestRedactLeavesCredentialFreeLinesAlone(t *testing.T) {
	line := "frame=  120 fps= 25 q=-1.0 size=    1024kB time=00:00:04.80"
	if got := redact(line); got != line {
		t.Errorf("redact modified a line with no credentials: %q", got)
	}
}
