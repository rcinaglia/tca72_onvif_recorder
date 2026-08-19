package onvifcam

import "testing"

func TestWithCredentials(t *testing.T) {
	cases := []struct {
		name     string
		rawURL   string
		user     string
		pass     string
		expected string
	}{
		{
			name:     "bare url gets credentials",
			rawURL:   "rtsp://192.168.1.10:554/stream1",
			user:     "admin",
			pass:     "secret",
			expected: "rtsp://admin:secret@192.168.1.10:554/stream1",
		},
		{
			name:     "already-authenticated url is left alone",
			rawURL:   "rtsp://someone:else@192.168.1.10:554/stream1",
			user:     "admin",
			pass:     "secret",
			expected: "rtsp://someone:else@192.168.1.10:554/stream1",
		},
		{
			name:     "special characters in the password are escaped",
			rawURL:   "rtsp://192.168.1.10:554/stream1",
			user:     "admin",
			pass:     "p@ss/w:rd",
			expected: "rtsp://admin:p%40ss%2Fw%3Ard@192.168.1.10:554/stream1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WithCredentials(tc.rawURL, tc.user, tc.pass)
			if err != nil {
				t.Fatalf("WithCredentials: %v", err)
			}
			if got != tc.expected {
				t.Errorf("WithCredentials(%q) = %q, want %q", tc.rawURL, got, tc.expected)
			}
		})
	}
}
