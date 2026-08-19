// Command nvr watches an ONVIF camera for events and, while events keep
// occurring, records its RTSP stream to disk. See config.json for tunables.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"nvr/config"
	"nvr/internal/app"
	"nvr/internal/onvifcam"
	"nvr/internal/storage"
)

// subscriptionLifetime is how long the camera is asked to keep a pull-point
// subscription alive without renewal. The listener renews it well before
// this expires (see events.go) and re-subscribes outright if a pull fails,
// so in normal operation this value is only a safety margin. It still
// matters if the process ever dies without a chance to unsubscribe (e.g.
// kill -9, power loss): some ONVIF stacks only allow one live subscription
// at a time, so a leaked one blocks new subscriptions until it expires.
const subscriptionLifetime = 5 * time.Minute

// pullTimeout is how long a single PullMessages call may block on the
// camera waiting for an event before returning empty.
const pullTimeout = 10 * time.Second

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatalf("ffmpeg not found in PATH: it is required to record the RTSP stream")
	}

	cfg, created, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if created {
		log.Printf("wrote default config to %s — fill in the camera section and run again", *configPath)
		return
	}
	if cfg.Camera.XAddr == "" || cfg.Camera.Username == "" {
		log.Fatalf("camera.xaddr and camera.username must be set in %s", *configPath)
	}

	db, err := storage.Open(cfg.Database.Path)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer db.Close()

	if err := db.Reconcile(cfg.Recording.OutputDir); err != nil {
		log.Printf("storage: reconcile after previous run: %v", err)
	}
	if err := db.EnforceLimit(cfg.Recording.OutputDir, cfg.Recording.MaxStorageBytes.Int64()); err != nil {
		log.Printf("storage: initial limit check: %v", err)
	}

	cam, err := onvifcam.Connect(cfg.Camera.XAddr, cfg.Camera.Username, cfg.Camera.Password)
	if err != nil {
		log.Fatalf("onvif: %v", err)
	}

	rtspURL, err := cam.StreamURI()
	if err != nil {
		log.Fatalf("onvif: %v", err)
	}
	log.Printf("onvif: rtsp stream is %s", rtspURL) // logged before creds are added in

	// GetStreamUri commonly returns a bare URL; the camera still expects
	// RTSP-level auth with the same credentials. Do this after logging so
	// the password never lands in the log.
	authedURL, err := onvifcam.WithCredentials(rtspURL, cfg.Camera.Username, cfg.Camera.Password)
	if err != nil {
		log.Fatalf("onvif: %v", err)
	}

	listener, err := cam.Subscribe(subscriptionLifetime, pullTimeout)
	if err != nil {
		log.Fatalf("onvif: subscribing to events: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("nvr: listening for events, idle timeout %s, storage limit %d bytes",
		cfg.IdleTimeout(), cfg.Recording.MaxStorageBytes.Int64())

	orch := app.New(authedURL, db, cfg)
	if err := orch.Run(ctx, listener); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("nvr: stopped: %v", err)
	}
}
