package onvifcam

import (
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/beevik/etree"
	"github.com/use-go/onvif"
	"github.com/use-go/onvif/gosoap"
	"github.com/use-go/onvif/networking"
	"github.com/use-go/onvif/xsd"
)

// Event is a single ONVIF notification pulled from the camera. We don't try
// to interpret vendor-specific topics/state semantics: any notification is
// treated as "something happened", which is all the recorder needs to know.
type Event struct {
	Topic string
	Time  time.Time
}

// Listener pulls messages from an ONVIF PullPoint subscription.
type Listener struct {
	cam              *Camera
	subscriptionAddr string
	subscriptionLife time.Duration
	pullTimeout      time.Duration
}

// NOTE on the manual XML structs below: the use-go/onvif typed request for
// CreatePullPointSubscription can't be used as-is. Its InitialTerminationTime
// field is an AbsoluteOrRelativeTimeType struct with two anonymous fields
// (xsd.DateTime and xsd.Duration); encoding/xml doesn't inline anonymous
// non-struct fields, so marshalling produces
// <wsnt:InitialTerminationTime><DateTime/><Duration>PT60S</Duration></...>
// instead of the plain text "PT60S" the camera expects, and it replies 400.
// We build the requests by hand instead, with fields that marshal to plain
// text, mirroring how the original prototype worked around the same issue.

type createPullPointSubscriptionReq struct {
	XMLName                xml.Name     `xml:"tev:CreatePullPointSubscription"`
	InitialTerminationTime xsd.Duration `xml:"wsnt:InitialTerminationTime"`
}

type subscriptionEnvelope struct {
	Body struct {
		CreatePullPointSubscriptionResponse struct {
			SubscriptionReference struct {
				Address string `xml:"Address"`
			} `xml:"SubscriptionReference"`
		} `xml:"CreatePullPointSubscriptionResponse"`
	} `xml:"Body"`
}

type pullMessagesReq struct {
	XMLName      xml.Name     `xml:"tev:PullMessages"`
	Timeout      xsd.Duration `xml:"tev:Timeout"`
	MessageLimit int          `xml:"tev:MessageLimit"`
}

type pullMessagesEnvelope struct {
	Body struct {
		PullMessagesResponse struct {
			NotificationMessage []struct {
				Topic struct {
					Value string `xml:",chardata"`
				} `xml:"Topic"`
			} `xml:"NotificationMessage"`
		} `xml:"PullMessagesResponse"`
	} `xml:"Body"`
}

type unsubscribeReq struct {
	XMLName xml.Name `xml:"wsnt:Unsubscribe"`
}

type renewReq struct {
	XMLName         xml.Name     `xml:"wsnt:Renew"`
	TerminationTime xsd.Duration `xml:"wsnt:TerminationTime"`
}

func isoSeconds(d time.Duration) xsd.Duration {
	return xsd.Duration(fmt.Sprintf("PT%dS", int(d.Seconds())))
}

// soapTransport never keeps connections alive. The event pull is a long
// poll (blocks on the camera for up to pullTimeout): many embedded ONVIF
// servers close what looks to them like an idle keep-alive socket as soon
// as they've answered it, but Go's transport doesn't find out until it
// hands that "idle" connection back out for the *next* request, which then
// fails with a bare EOF before anything is even sent. Disabling keep-alive
// forces a fresh connection per call and avoids that race entirely; the
// extra connection setup is irrelevant next to a several-second poll.
var soapTransport = &http.Transport{DisableKeepAlives: true}

func (c *Camera) sendSoap(addr string, body any, timeout time.Duration) (*http.Response, error) {
	bodyXML, err := xml.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(bodyXML); err != nil {
		return nil, fmt.Errorf("building request document: %w", err)
	}

	soap := gosoap.NewEmptySOAP()
	soap.AddBodyContent(doc.Root())
	soap.AddRootNamespaces(onvif.Xlmns)
	soap.AddWSSecurity(c.params.Username, c.params.Password)

	client := &http.Client{Timeout: timeout, Transport: soapTransport}
	return networking.SendSoap(client, addr, soap.String())
}

// Subscribe creates a new PullPoint event subscription and returns a
// Listener for it. lifetime controls how long the camera keeps the
// subscription alive without renewal; the Listener re-subscribes on its own
// if a pull fails, so a modest lifetime (a few minutes) is fine.
func (c *Camera) Subscribe(lifetime, pullTimeout time.Duration) (*Listener, error) {
	resp, err := c.sendSoap(c.device.GetEndpoint("events"), createPullPointSubscriptionReq{
		InitialTerminationTime: isoSeconds(lifetime),
	}, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("creating pull point subscription: %w", err)
	}
	sub, err := parseOnvifResponse[subscriptionEnvelope](resp)
	if err != nil {
		return nil, err
	}
	addr := sub.Body.CreatePullPointSubscriptionResponse.SubscriptionReference.Address
	if addr == "" {
		return nil, fmt.Errorf("device returned an empty subscription address")
	}
	return &Listener{
		cam:              c,
		subscriptionAddr: addr,
		subscriptionLife: lifetime,
		pullTimeout:      pullTimeout,
	}, nil
}

func (l *Listener) pullMessages() (*pullMessagesEnvelope, error) {
	resp, err := l.cam.sendSoap(l.subscriptionAddr, pullMessagesReq{
		Timeout:      isoSeconds(l.pullTimeout),
		MessageLimit: 50,
	}, l.pullTimeout+10*time.Second)
	if err != nil {
		return nil, err
	}
	return parseOnvifResponse[pullMessagesEnvelope](resp)
}

func (l *Listener) unsubscribe() {
	resp, err := l.cam.sendSoap(l.subscriptionAddr, unsubscribeReq{}, 10*time.Second)
	if err != nil {
		return // best effort: the subscription will simply expire on its own
	}
	resp.Body.Close()
}

// renew asks the camera to push the subscription's expiry back out to a
// full subscriptionLife from now, so a long-lived listener doesn't have to
// rely on the subscription happening to auto-renew on every pull (not all
// ONVIF stacks do that) nor let it run out and force a resubscribe.
func (l *Listener) renew() error {
	resp, err := l.cam.sendSoap(l.subscriptionAddr, renewReq{
		TerminationTime: isoSeconds(l.subscriptionLife),
	}, 15*time.Second)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Run pulls events until ctx is cancelled, invoking onEvent for each
// notification received. On pull errors (e.g. an expired subscription) it
// transparently re-subscribes with backoff instead of giving up.
func (l *Listener) Run(ctx context.Context, onEvent func(Event)) error {
	defer l.unsubscribe()

	// Some embedded ONVIF stacks (this one included, apparently) don't
	// really implement the "block up to Timeout, then return empty" long
	// poll: a PullMessages call with nothing to report just gets the
	// connection reset (EOF) almost immediately instead of waiting, while a
	// call that already has something queued answers fine. New events are
	// still picked up correctly the moment they happen, so this is just
	// what "no events right now" looks like on this camera, not a failure —
	// it's not logged, and the subscription is left alone. quietSince is
	// non-zero while such a streak of failed pulls is ongoing;
	// resubscribeAfterQuiet is only a safety net for the case a failure
	// streak never recovers, which would otherwise mean silently never
	// getting events again.
	const retryDelay = 2 * time.Second
	const resubscribeAfterQuiet = 2 * time.Minute
	const maxBackoff = 30 * time.Second
	var quietSince time.Time
	backoff := time.Second

	// minPullInterval floors the gap between the start of one PullMessages
	// call and the next. PullMessages only blocks for pullTimeout when the
	// camera has nothing queued; if it already has a backlog it answers
	// instantly, and this camera has been observed emitting the same
	// "People"/"Motion" notification many times a second while active. With
	// no floor, that turns into a back-to-back HTTP request storm — with
	// keep-alive disabled that's a fresh TCP handshake every call — which is
	// enough to overwhelm a cheap embedded camera's HTTP stack (it starts
	// resetting connections) and can starve the RTSP service running on the
	// same device too. We don't need sub-second event granularity for
	// deciding "keep recording", so pace pulls no faster than this.
	const minPullInterval = 500 * time.Millisecond

	// Renew at the halfway point of the subscription's lifetime rather than
	// waiting for it to nearly lapse, so a slow renew or a missed cycle
	// still leaves margin before the camera would drop the subscription.
	renewEvery := l.subscriptionLife / 2
	lastRenew := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		pullStart := time.Now()
		resp, err := l.pullMessages()
		if err != nil {
			if quietSince.IsZero() {
				quietSince = time.Now()
			}

			if time.Since(quietSince) < resubscribeAfterQuiet {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(retryDelay):
				}
				continue
			}

			// Only reachable after failing continuously for
			// resubscribeAfterQuiet: this is no longer "camera has nothing
			// to say", it's worth both a log line and actually rebuilding
			// the subscription in case that really is what's wrong.
			log.Printf("onvif: no successful pull in over %s (%v); resubscribing as a precaution", resubscribeAfterQuiet, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}

			// The subscription itself may or may not still be valid, but
			// dropping it before asking for a new one avoids hitting a
			// "one subscription at a time" limit on devices that enforce
			// one.
			l.unsubscribe()

			fresh, subErr := l.cam.Subscribe(l.subscriptionLife, l.pullTimeout)
			if subErr != nil {
				log.Printf("onvif: resubscribe failed: %v", subErr)
				if backoff < maxBackoff {
					backoff *= 2
				}
				continue
			}
			l.subscriptionAddr = fresh.subscriptionAddr
			backoff = time.Second
			quietSince = time.Now() // give the fresh subscription its own grace window
			lastRenew = time.Now()
			continue
		}

		quietSince = time.Time{}
		backoff = time.Second

		for _, nm := range resp.Body.PullMessagesResponse.NotificationMessage {
			onEvent(Event{Topic: nm.Topic.Value, Time: time.Now()})
		}

		if time.Since(lastRenew) >= renewEvery {
			if err := l.renew(); err != nil {
				// Not fatal: if the subscription actually lapses, the next
				// pull will fail and the resubscribe path takes over.
				log.Printf("onvif: renewing subscription failed: %v", err)
			} else {
				lastRenew = time.Now()
			}
		}

		if elapsed := time.Since(pullStart); elapsed < minPullInterval {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(minPullInterval - elapsed):
			}
		}
	}
}
