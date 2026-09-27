package main

import (
	"errors"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureIndex      = "receivesmsco_active_numbers.html"
	fixtureNumberPage = "receivesmsco_au_22630.html"
	fixtureNumberPath = "/au-phone-number/22630/"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := ioutil.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// newFixtureServer serves the saved receivesms.co captures at the same paths
// the live site uses, so the provider is exercised through real HTTP.
func newFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	index := fixture(t, fixtureIndex)
	page := fixture(t, fixtureNumberPage)
	mux := http.NewServeMux()
	mux.HandleFunc(receiveSMSCoIndexPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})
	mux.HandleFunc(fixtureNumberPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestListNumbersParsesIndex(t *testing.T) {
	server := newFixtureServer(t)
	provider := NewReceiveSMSCo(server.Client(), server.URL)

	numbers, err := provider.ListNumbers()
	if err != nil {
		t.Fatalf("ListNumbers: %v", err)
	}
	if len(numbers) != 30 {
		t.Fatalf("got %d numbers, want 30", len(numbers))
	}

	first := numbers[0]
	if first.Number != "+61420157021" {
		t.Errorf("first.Number = %q, want +61420157021", first.Number)
	}
	if first.Country != "AU" {
		t.Errorf("first.Country = %q, want AU", first.Country)
	}
	if want := server.URL + fixtureNumberPath; first.URL != want {
		t.Errorf("first.URL = %q, want %q", first.URL, want)
	}
	if first.CreatedAt == "" {
		t.Errorf("first.CreatedAt is empty")
	}
	for i, n := range numbers {
		if n.URL == "" || n.Number == "" || n.Country == "" {
			t.Errorf("number %d incomplete: %+v", i, n)
		}
		if !strings.HasPrefix(n.Number, "+") || strings.ContainsAny(n.Number, " -()") {
			t.Errorf("number %d not normalised: %q", i, n.Number)
		}
	}
}

func TestListNumbersAbsoluteURLUsesLiveBase(t *testing.T) {
	// The committed default must point at the live site, not a test server.
	if defaultProvider.(*ReceiveSMSCo).baseURL != "https://receivesms.co" {
		t.Fatalf("defaultProvider baseURL = %q", defaultProvider.(*ReceiveSMSCo).baseURL)
	}
	got, err := resolveURL("https://receivesms.co/active-numbers", fixtureNumberPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://receivesms.co" + fixtureNumberPath; got != want {
		t.Errorf("resolveURL = %q, want %q", got, want)
	}
}

func TestMessagesParsesNumberPage(t *testing.T) {
	server := newFixtureServer(t)
	provider := NewReceiveSMSCo(server.Client(), server.URL)

	messages, err := provider.Messages(Number{Number: "+61420157021", Country: "AU", URL: server.URL + fixtureNumberPath})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) == 0 {
		t.Fatal("got no messages")
	}

	first := messages[0]
	if first.Originator != "Uber" {
		t.Errorf("first.Originator = %q, want Uber", first.Originator)
	}
	if !strings.Contains(first.Body, "Your code: 5973") {
		t.Errorf("first.Body = %q, want it to contain 'Your code: 5973'", first.Body)
	}
	if first.CreatedAt != "8 seconds ago" {
		t.Errorf("first.CreatedAt = %q, want '8 seconds ago'", first.CreatedAt)
	}
	if len(messages) > 1 && messages[1].Originator != "TWVerify" {
		t.Errorf("second.Originator = %q, want TWVerify (site order not preserved)", messages[1].Originator)
	}
	for i, m := range messages {
		if m.Originator == "" || m.Body == "" {
			t.Errorf("message %d incomplete: %+v", i, m)
		}
	}
}

func TestMessagesLegacyNumberWithoutURL(t *testing.T) {
	server := newFixtureServer(t)
	provider := NewReceiveSMSCo(server.Client(), server.URL)

	_, err := provider.Messages(Number{Number: "+447510080141", Country: "United Kingdom"})
	var legacy *LegacyNumberError
	if !errors.As(err, &legacy) {
		t.Fatalf("err = %v, want *LegacyNumberError", err)
	}
	if !strings.Contains(err.Error(), "saved under a previous provider") {
		t.Errorf("unexpected message: %q", err.Error())
	}
}

func TestCloudflareChallengeIsATypedErrorNotAPanic(t *testing.T) {
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("<!DOCTYPE html><html><head><title>Just a moment...</title></head><body><div id=\"challenge\"></div></body></html>"))
	}
	server := httptest.NewServer(http.HandlerFunc(challenge))
	defer server.Close()
	provider := NewReceiveSMSCo(server.Client(), server.URL)

	_, err := provider.ListNumbers()
	var ce *ChallengeError
	if !errors.As(err, &ce) {
		t.Fatalf("ListNumbers err = %v, want *ChallengeError", err)
	}
	if !strings.Contains(err.Error(), "Cloudflare challenge") {
		t.Errorf("unexpected message: %q", err.Error())
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("challenge error must be a single line: %q", err.Error())
	}

	_, err = provider.Messages(Number{Number: "+1", URL: server.URL + "/x/"})
	if !errors.As(err, &ce) {
		t.Fatalf("Messages err = %v, want *ChallengeError", err)
	}
}

func TestCloudflareChallengeDetectedByTitleWithoutHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("<html><head><title>Just a moment...</title></head><body></body></html>"))
	}))
	defer server.Close()

	_, err := NewReceiveSMSCo(server.Client(), server.URL).ListNumbers()
	var ce *ChallengeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ChallengeError", err)
	}
}

func TestNonSuccessStatusIsAnHTTPStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := NewReceiveSMSCo(server.Client(), server.URL).ListNumbers()
	var se *HTTPStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *HTTPStatusError", err)
	}
	if se.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", se.Status)
	}
}

func TestUnexpectedMarkupIsAParseError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body><p>redesigned</p></body></html>"))
	}))
	defer server.Close()
	provider := NewReceiveSMSCo(server.Client(), server.URL)

	var pe *ParseError
	if _, err := provider.ListNumbers(); !errors.As(err, &pe) {
		t.Errorf("ListNumbers err = %v, want *ParseError", err)
	}
	if _, err := provider.Messages(Number{Number: "+1", URL: server.URL + "/x/"}); !errors.As(err, &pe) {
		t.Errorf("Messages err = %v, want *ParseError", err)
	}
}

func TestNormalizeNumber(t *testing.T) {
	cases := map[string]string{
		"+61 420 157 021":      "+61420157021",
		"  +1 (415) 237-0403 ": "+14152370403",
		"44 7510 080141":       "447510080141",
		"":                     "",
	}
	for in, want := range cases {
		if got := normalizeNumber(in); got != want {
			t.Errorf("normalizeNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestCarriesUserAgent(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		w.Write(fixture(t, fixtureIndex))
	}))
	defer server.Close()

	if _, err := NewReceiveSMSCo(server.Client(), server.URL).ListNumbers(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seen, "fake-sms/") || !strings.Contains(seen, "github.com/Narasimha1997/fake-sms") {
		t.Errorf("User-Agent = %q", seen)
	}
}

// TestLiveReceiveSMSCo hits the real site. Opt in with FAKE_SMS_LIVE=1.
func TestLiveReceiveSMSCo(t *testing.T) {
	if os.Getenv("FAKE_SMS_LIVE") != "1" {
		t.Skip("set FAKE_SMS_LIVE=1 to run the live smoke test")
	}

	numbers, err := defaultProvider.ListNumbers()
	if err != nil {
		t.Fatalf("live ListNumbers: %v", err)
	}
	if len(numbers) == 0 {
		t.Fatal("live ListNumbers returned no numbers")
	}
	t.Logf("live: %d numbers; first %s (%s) -> %s", len(numbers), numbers[0].Number, numbers[0].Country, numbers[0].URL)

	messages, err := defaultProvider.Messages(numbers[0])
	if err != nil {
		t.Fatalf("live Messages(%s): %v", numbers[0].Number, err)
	}
	if len(messages) == 0 {
		t.Fatalf("live Messages(%s) returned no messages", numbers[0].Number)
	}
	t.Logf("live: %d messages; first from %q at %q: %q", len(messages), messages[0].Originator, messages[0].CreatedAt, messages[0].Body)
}
