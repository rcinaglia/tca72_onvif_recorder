// Package onvifcam wraps the parts of the ONVIF protocol this program needs:
// fetching the RTSP stream URI and subscribing/pulling the event stream.
package onvifcam

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/use-go/onvif"
	onvifmedia "github.com/use-go/onvif/media"
	xsdonvif "github.com/use-go/onvif/xsd/onvif"
)

// Camera is a connected ONVIF device.
type Camera struct {
	device *onvif.Device
	params onvif.DeviceParams
}

// Connect logs into the ONVIF device at xaddr.
func Connect(xaddr, username, password string) (*Camera, error) {
	params := onvif.DeviceParams{Xaddr: xaddr, Username: username, Password: password}
	device, err := onvif.NewDevice(params)
	if err != nil {
		return nil, fmt.Errorf("connecting to onvif device: %w", err)
	}
	return &Camera{device: device, params: params}, nil
}

func parseOnvifResponse[T any](resp *http.Response) (*T, error) {
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("onvif http error %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading onvif response body: %w", err)
	}

	var result T
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing onvif response: %w", err)
	}
	return &result, nil
}

type profilesEnvelope struct {
	Body struct {
		GetProfilesResponse struct {
			Profiles []struct {
				Token string `xml:"token,attr"`
				Name  string `xml:"Name"`
			} `xml:"Profiles"`
		} `xml:"GetProfilesResponse"`
	} `xml:"Body"`
}

type streamUriEnvelope struct {
	Body struct {
		GetStreamUriResponse struct {
			MediaUri struct {
				Uri string `xml:"Uri"`
			} `xml:"MediaUri"`
		} `xml:"GetStreamUriResponse"`
	} `xml:"Body"`
}

// StreamURI fetches the RTSP URI of the device's first media profile.
func (c *Camera) StreamURI() (string, error) {
	resp, err := c.device.CallMethod(onvifmedia.GetProfiles{})
	if err != nil {
		return "", fmt.Errorf("getting media profiles: %w", err)
	}
	profiles, err := parseOnvifResponse[profilesEnvelope](resp)
	if err != nil {
		return "", err
	}
	if len(profiles.Body.GetProfilesResponse.Profiles) == 0 {
		return "", fmt.Errorf("device has no media profiles")
	}
	profileToken := profiles.Body.GetProfilesResponse.Profiles[0].Token

	req := onvifmedia.GetStreamUri{
		ProfileToken: xsdonvif.ReferenceToken(profileToken),
		StreamSetup: xsdonvif.StreamSetup{
			Stream: "RTP-Unicast",
			Transport: xsdonvif.Transport{
				Protocol: "RTSP",
			},
		},
	}
	resp, err = c.device.CallMethod(req)
	if err != nil {
		return "", fmt.Errorf("getting stream uri: %w", err)
	}
	streamResp, err := parseOnvifResponse[streamUriEnvelope](resp)
	if err != nil {
		return "", err
	}
	uri := streamResp.Body.GetStreamUriResponse.MediaUri.Uri
	if uri == "" {
		return "", fmt.Errorf("device returned an empty stream uri")
	}
	return uri, nil
}

// WithCredentials returns rawURL with username/password embedded as RTSP
// userinfo (rtsp://user:pass@host/path). GetStreamUri commonly returns a
// bare URL and expects the client to authenticate at the RTSP level with
// the same credentials used for ONVIF; ffmpeg (and RTSP clients generally)
// have no separate flag for that; the only place to put them is the URL. If
// rawURL already carries userinfo, it's left untouched.
func WithCredentials(rawURL, username, password string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parsing stream uri: %w", err)
	}
	if u.User == nil && username != "" {
		u.User = url.UserPassword(username, password)
	}
	return u.String(), nil
}
