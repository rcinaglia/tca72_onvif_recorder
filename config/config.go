// Package config loads and represents the NVR's on-disk JSON configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ByteSize is an int64 number of bytes that can also be unmarshalled from
// human-friendly strings like "10GB" or "500MB", so the config file stays
// readable.
type ByteSize int64

func (b ByteSize) Int64() int64 { return int64(b) }

func (b *ByteSize) UnmarshalJSON(data []byte) error {
	// Accept a plain JSON number (bytes).
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = ByteSize(n)
		return nil
	}

	// Otherwise accept a string like "10GB", "512MB", "1024" ...
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("max_storage_bytes: invalid value %s: %w", data, err)
	}
	v, err := parseByteSize(s)
	if err != nil {
		return err
	}
	*b = ByteSize(v)
	return nil
}

func (b ByteSize) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(b))
}

func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty byte size")
	}
	// Longest suffixes first so "GB" isn't mistaken for the generic "B" suffix.
	units := []struct {
		suffix string
		mult   int64
	}{
		{"TB", 1 << 40},
		{"GB", 1 << 30},
		{"MB", 1 << 20},
		{"KB", 1 << 10},
		{"B", 1},
	}
	upper := strings.ToUpper(s)
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			numPart := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			if numPart == "" {
				continue
			}
			f, err := strconv.ParseFloat(numPart, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	return n, nil
}

// Config is the full on-disk configuration for the NVR.
type Config struct {
	Camera struct {
		XAddr    string `json:"xaddr"` // e.g. "192.168.1.10:80"
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"camera"`

	Recording struct {
		// Folder recordings are written to.
		OutputDir string `json:"output_dir"`

		// How many seconds without a new ONVIF event before an in-progress
		// recording is stopped. Changeable at will, read fresh on every start.
		EventIdleTimeoutSeconds int `json:"event_idle_timeout_seconds"`

		// Maximum total size the output folder is allowed to reach. Once at
		// or over this limit the oldest recordings are deleted to make room.
		// Accepts a plain byte count or a human string such as "10GB".
		MaxStorageBytes ByteSize `json:"max_storage_bytes"`
	} `json:"recording"`

	Database struct {
		// Path to the SQLite file used to track recordings.
		Path string `json:"path"`
	} `json:"database"`
}

// IdleTimeout returns the configured idle timeout as a time.Duration.
func (c *Config) IdleTimeout() time.Duration {
	return time.Duration(c.Recording.EventIdleTimeoutSeconds) * time.Second
}

const defaultConfigTemplate = `{
  "camera": {
    "xaddr": "",
    "username": "",
    "password": ""
  },
  "recording": {
    "output_dir": "./recordings",
    "event_idle_timeout_seconds": 10,
    "max_storage_bytes": "10GB"
  },
  "database": {
    "path": "./nvr.db"
  }
}
`

// Load reads the config at path. If the file does not exist, a default
// config file is written there and created=true is returned so the caller
// can prompt the user to fill in camera credentials before starting.
func Load(path string) (cfg *Config, created bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if werr := os.WriteFile(path, []byte(defaultConfigTemplate), 0o644); werr != nil {
			return nil, false, fmt.Errorf("writing default config: %w", werr)
		}
		data = []byte(defaultConfigTemplate)
		created = true
	} else if err != nil {
		return nil, false, fmt.Errorf("reading config: %w", err)
	}

	cfg = &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, false, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if cfg.Recording.OutputDir == "" {
		cfg.Recording.OutputDir = "./recordings"
	}
	if cfg.Recording.EventIdleTimeoutSeconds <= 0 {
		cfg.Recording.EventIdleTimeoutSeconds = 10
	}
	if cfg.Recording.MaxStorageBytes <= 0 {
		cfg.Recording.MaxStorageBytes = ByteSize(10 << 30) // 10GB
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "./nvr.db"
	}

	return cfg, created, nil
}
