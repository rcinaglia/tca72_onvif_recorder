// Package recorder starts and stops ffmpeg processes that save an RTSP
// stream to disk.
package recorder

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Recording is a single in-progress or finished ffmpeg capture.
type Recording struct {
	cmd       *exec.Cmd
	Path      string
	StartedAt time.Time
	done      chan struct{}
	waitErr   error
}

// credentialsInURL matches the userinfo part of an rtsp:// URL, so it can be
// stripped out of anything ffmpeg prints before that reaches a log or
// terminal. ffmpeg echoes the input URL verbatim in several of its own
// diagnostic and error lines (e.g. "Error opening input file rtsp://...");
// since the URL carries the camera's real credentials (see
// onvifcam.WithCredentials), those lines would otherwise leak the password.
var credentialsInURL = regexp.MustCompile(`rtsp://[^@/\s]+@`)

func redact(line string) string {
	return credentialsInURL.ReplaceAllString(line, "rtsp://***:***@")
}

// Start begins remuxing rtspURL into a new file under outputDir, named after
// the current date and time. It uses "-c copy": ffmpeg only repackages the
// camera's already-encoded H.264/H.265 stream, it never decodes or
// re-encodes it, so CPU use is minimal (a copy, not a transcode).
//
// The container is Matroska (.mkv) rather than MP4: MP4's index (moov atom)
// is normally written only when the file is closed cleanly, so a process
// killed mid-recording leaves an unplayable file unless extra flags are
// used. Matroska is designed to remain playable even if writing stops
// abruptly, which fits a stream that gets cut off whenever events stop.
func Start(rtspURL, outputDir string) (*Recording, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating output dir: %w", err)
	}

	start := time.Now()
	filename := start.Format("2006-01-02_15-04-05") + ".mkv"
	path := filepath.Join(outputDir, filename)

	cmd := exec.Command("ffmpeg",
		"-nostdin",
		"-loglevel", "warning",
		"-rtsp_transport", "tcp",
		"-i", rtspURL,
		"-c", "copy",
		"-f", "matroska",
		path,
	)
	cmd.Stdin = nil

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("piping ffmpeg stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("ffmpeg[%s]: %s", filename, redact(scanner.Text()))
		}
	}()

	r := &Recording{
		cmd:       cmd,
		Path:      path,
		StartedAt: start,
		done:      make(chan struct{}),
	}
	go func() {
		r.waitErr = cmd.Wait()
		close(r.done)
	}()
	return r, nil
}

// Stop asks ffmpeg to finish the file gracefully (SIGINT makes ffmpeg flush
// buffers and close the container properly) and waits up to timeout before
// force-killing it.
func (r *Recording) Stop(timeout time.Duration) error {
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(syscall.SIGINT)
	}
	select {
	case <-r.done:
	case <-time.After(timeout):
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		<-r.done
	}
	return r.waitErr
}

// Size returns the current on-disk size of the recording file.
func (r *Recording) Size() int64 {
	fi, err := os.Stat(r.Path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
