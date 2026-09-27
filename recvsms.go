package main

import (
	"fmt"
	"net/http"
	"time"
)

// version is stamped into the User-Agent so the provider can identify the
// tool. Override at build time with -ldflags "-X main.version=1.2.3".
var version = "dev"

const (
	httpTimeout   = 20 * time.Second
	userAgentBase = "fake-sms/%s (+https://github.com/Narasimha1997/fake-sms)"
)

// Provider is the seam between the CLI and whichever temporary-number
// website is currently scraped. Implementations must never panic or exit;
// every failure is returned as an error for main.go to report.
type Provider interface {
	// ListNumbers returns the numbers currently offered by the provider, in
	// site order. Each Number carries the absolute URL of its messages page.
	ListNumbers() ([]Number, error)
	// Messages returns the messages received by n, in site order.
	Messages(n Number) ([]Message, error)
}

// HTTPStatusError is returned when the provider answers with a non-2xx
// status that is not a Cloudflare challenge.
type HTTPStatusError struct {
	URL    string
	Status int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("provider returned HTTP %d for %s", e.Status, e.URL)
}

// ChallengeError is returned when the provider sits behind a Cloudflare
// managed challenge that cannot be solved by a non-interactive client.
// This is exactly the condition that used to crash the tool.
type ChallengeError struct {
	URL string
}

func (e *ChallengeError) Error() string {
	return fmt.Sprintf("provider %s is behind a Cloudflare challenge and cannot be scraped by this tool right now", e.URL)
}

// ParseError is returned when a page was fetched but its markup no longer
// matches what the scraper expects (usually a site redesign).
type ParseError struct {
	URL    string
	Reason string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("could not parse %s: %s", e.URL, e.Reason)
}

// LegacyNumberError is returned when a Number saved by a previous provider
// (no messages-page URL) is used against the current one.
type LegacyNumberError struct {
	Number string
}

func (e *LegacyNumberError) Error() string {
	return fmt.Sprintf("number %s was saved under a previous provider — remove it and add it again", e.Number)
}

func userAgent() string {
	return fmt.Sprintf(userAgentBase, version)
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// defaultProvider is the provider used by the interactive CLI.
var defaultProvider Provider = NewReceiveSMSCo(newHTTPClient(), receiveSMSCoBaseURL)
