// Package app wires together the ONVIF event listener, the ffmpeg recorder
// and the storage janitor into the NVR's main run loop.
package app

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"nvr/config"
	"nvr/internal/onvifcam"
	"nvr/internal/recorder"
	"nvr/internal/storage"
)

// janitorInterval is how often the storage limit is re-checked while idle or
// mid-recording, on top of the check that always runs right after a
// recording finishes. This catches a single long recording that alone grows
// past the limit.
const janitorInterval = time.Minute

// stopGrace is how long a recording is given to close its file cleanly
// after being asked to stop before it gets force-killed.
const stopGrace = 5 * time.Second

// listenerShutdownGrace bounds how long shutdown waits for the ONVIF
// listener to unsubscribe before giving up and exiting anyway.
const listenerShutdownGrace = 30 * time.Second

// Orchestrator turns ONVIF events into recordings and keeps the output
// folder within its configured size limit.
type Orchestrator struct {
	rtspURL   string
	db        *storage.DB
	outputDir string
	idle      time.Duration
	maxBytes  int64
}

// New builds an Orchestrator from a loaded config.
func New(rtspURL string, db *storage.DB, cfg *config.Config) *Orchestrator {
	return &Orchestrator{
		rtspURL:   rtspURL,
		db:        db,
		outputDir: cfg.Recording.OutputDir,
		idle:      cfg.IdleTimeout(),
		maxBytes:  cfg.Recording.MaxStorageBytes.Int64(),
	}
}

// Run drives the event -> record -> enforce-limit loop until ctx is
// cancelled. It also runs the ONVIF listener in the background.
func (o *Orchestrator) Run(ctx context.Context, listener *onvifcam.Listener) error {
	events := make(chan onvifcam.Event, 32)
	listenerDone := make(chan error, 1)
	go func() {
		listenerDone <- listener.Run(ctx, func(ev onvifcam.Event) {
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		})
	}()

	var current *recorder.Recording
	var currentID int64

	idleTimer := time.NewTimer(o.idle)
	stopIdleTimer(idleTimer, false)

	janitor := time.NewTicker(janitorInterval)
	defer janitor.Stop()

	stopCurrent := func(reason string) {
		if current == nil {
			return
		}
		log.Printf("recorder: stopping %s (%s)", current.Path, reason)
		stopErr := current.Stop(stopGrace)
		size := current.Size()
		switch {
		case stopErr == nil:
			log.Printf("recorder: saved %s (%d bytes)", current.Path, size)
		case size > 0:
			// We asked ffmpeg to stop by sending it SIGINT, so a non-zero
			// exit here is expected, not necessarily a failure — ffmpeg's
			// own exit code on a caught signal varies by version/build. A
			// non-empty file is the actual sign the recording is usable,
			// so only note this, don't alarm on it.
			log.Printf("recorder: saved %s (%d bytes); ffmpeg reported: %v", current.Path, size, stopErr)
		default:
			// Empty output is a real problem: nothing worth keeping.
			log.Printf("recorder: %s is empty after ffmpeg exited: %v", current.Path, stopErr)
		}
		if err := o.db.FinishRecording(currentID, time.Now(), size); err != nil {
			log.Printf("storage: failed to finalize recording %d: %v", currentID, err)
		}
		if err := o.db.EnforceLimit(o.outputDir, o.maxBytes); err != nil {
			log.Printf("storage: enforcing limit failed: %v", err)
		}
		current = nil
	}

	for {
		select {
		case <-ctx.Done():
			stopIdleTimer(idleTimer, true)
			stopCurrent("shutting down")
			// Wait for the listener to actually exit: its deferred
			// unsubscribe only runs once Run() returns, and the camera
			// needs to hear that Unsubscribe before we exit the process,
			// or the subscription is left dangling on the device until it
			// expires on its own.
			select {
			case <-listenerDone:
			case <-time.After(listenerShutdownGrace):
				log.Printf("onvif: listener did not shut down within %s, exiting anyway", listenerShutdownGrace)
			}
			return ctx.Err()

		case ev := <-events:
			log.Printf("onvif: event %q", ev.Topic)
			if current == nil {
				rec, err := recorder.Start(o.rtspURL, o.outputDir)
				if err != nil {
					log.Printf("recorder: failed to start: %v", err)
					continue
				}
				id, err := o.db.InsertRecording(rec.Path, filepath.Base(rec.Path), rec.StartedAt)
				if err != nil {
					log.Printf("storage: failed to record start of %s: %v", rec.Path, err)
				}
				log.Printf("recorder: started %s", rec.Path)
				current = rec
				currentID = id
			}
			stopIdleTimer(idleTimer, true)
			idleTimer.Reset(o.idle)

		case <-idleTimer.C:
			stopCurrent("no events for the configured idle timeout")

		case <-janitor.C:
			if err := o.db.EnforceLimit(o.outputDir, o.maxBytes); err != nil {
				log.Printf("storage: enforcing limit failed: %v", err)
			}

		case err := <-listenerDone:
			stopIdleTimer(idleTimer, true)
			stopCurrent("event listener stopped")
			return err
		}
	}
}

// stopIdleTimer stops t, draining its channel if it had already fired, so it
// can be safely reused with Reset. When wasRunning is false the timer is
// assumed to have never been started (its channel can't have anything in
// it), matching the state right after time.NewTimer.
func stopIdleTimer(t *time.Timer, wasRunning bool) {
	if !t.Stop() && wasRunning {
		select {
		case <-t.C:
		default:
		}
	}
}
