package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anaskhan96/soup"
)

const (
	receiveSMSCoBaseURL     = "https://receivesms.co"
	receiveSMSCoIndexPath   = "/active-numbers"
	receiveSMSCoMaxBodySize = 8 << 20 // 8 MiB; pages are ~60 KiB

	cfMitigatedHeader    = "Cf-Mitigated"
	cfMitigatedChallenge = "challenge"
	cfChallengeTitle     = "<title>Just a moment..."
)

// ReceiveSMSCo scrapes https://receivesms.co (plain server-rendered HTML).
type ReceiveSMSCo struct {
	client  *http.Client
	baseURL string
}

// NewReceiveSMSCo builds a provider that talks to baseURL through client.
func NewReceiveSMSCo(client *http.Client, baseURL string) *ReceiveSMSCo {
	return &ReceiveSMSCo{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// ListNumbers implements Provider.
func (p *ReceiveSMSCo) ListNumbers() ([]Number, error) {
	indexURL := p.baseURL + receiveSMSCoIndexPath
	document, err := p.fetchDocument(indexURL)
	if err != nil {
		return nil, err
	}

	cards := document.FindAll("a", "class", "card-link")
	if len(cards) == 0 {
		return nil, &ParseError{URL: indexURL, Reason: "no number cards found (site markup changed?)"}
	}

	numbers := make([]Number, 0, len(cards))
	for idx, card := range cards {
		number, err := parseNumberCard(card, indexURL)
		if err != nil {
			log.Printf("skipping number card %d: %v", idx, err)
			continue
		}
		numbers = append(numbers, number)
	}
	return numbers, nil
}

// Messages implements Provider.
func (p *ReceiveSMSCo) Messages(n Number) ([]Message, error) {
	if strings.TrimSpace(n.URL) == "" {
		return nil, &LegacyNumberError{Number: n.Number}
	}

	document, err := p.fetchDocument(n.URL)
	if err != nil {
		return nil, err
	}

	list := document.Find("section", "class", "entry-list")
	if list.Error != nil {
		return nil, &ParseError{URL: n.URL, Reason: "no message list found (site markup changed?)"}
	}

	entries := list.FindAll("article", "class", "entry-card")
	messages := make([]Message, 0, len(entries))
	for idx, entry := range entries {
		message, err := parseMessageEntry(entry)
		if err != nil {
			log.Printf("skipping message %d on %s: %v", idx, n.URL, err)
			continue
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// fetchDocument GETs pageURL and returns the parsed DOM. It validates the
// HTTP status and detects Cloudflare challenges before any parsing happens.
func (p *ReceiveSMSCo) fetchDocument(pageURL string) (soup.Root, error) {
	body, err := p.fetch(pageURL)
	if err != nil {
		return soup.Root{}, err
	}

	document := soup.HTMLParse(body)
	if document.Error != nil {
		return soup.Root{}, &ParseError{URL: pageURL, Reason: document.Error.Error()}
	}
	return document, nil
}

func (p *ReceiveSMSCo) fetch(pageURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return "", fmt.Errorf("building request for %s: %w", pageURL, err)
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "text/html")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting %s: %w", pageURL, err)
	}
	defer resp.Body.Close()

	if resp.Header.Get(cfMitigatedHeader) == cfMitigatedChallenge {
		return "", &ChallengeError{URL: pageURL}
	}

	raw, err := ioutil.ReadAll(io.LimitReader(resp.Body, receiveSMSCoMaxBodySize))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", pageURL, err)
	}
	body := string(raw)

	if strings.Contains(body, cfChallengeTitle) {
		return "", &ChallengeError{URL: pageURL}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &HTTPStatusError{URL: pageURL, Status: resp.StatusCode}
	}
	return body, nil
}

// parseNumberCard turns one <a class="card card-link"> into a Number.
func parseNumberCard(card soup.Root, indexURL string) (Number, error) {
	href := strings.TrimSpace(card.Attrs()["href"])
	if href == "" {
		return Number{}, fmt.Errorf("card has no href")
	}
	pageURL, err := resolveURL(indexURL, href)
	if err != nil {
		return Number{}, err
	}

	flag := card.Find("img", "class", "flag")
	if flag.Error != nil {
		return Number{}, fmt.Errorf("card %s has no flag: %w", href, flag.Error)
	}
	country := strings.ToUpper(strings.TrimSpace(flag.Attrs()["alt"]))
	if country == "" {
		return Number{}, fmt.Errorf("card %s has an empty country code", href)
	}

	digits := card.Find("strong")
	if digits.Error != nil {
		return Number{}, fmt.Errorf("card %s has no number: %w", href, digits.Error)
	}
	number := normalizeNumber(digits.FullText())
	if number == "" {
		return Number{}, fmt.Errorf("card %s has an empty number", href)
	}

	return Number{
		Country:   country,
		Number:    number,
		URL:       pageURL,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05 Monday"),
	}, nil
}

// parseMessageEntry turns one <article class="entry-card"> into a Message.
func parseMessageEntry(entry soup.Root) (Message, error) {
	body := entry.Find("div", "class", "entry-body")
	if body.Error != nil {
		return Message{}, fmt.Errorf("entry has no body: %w", body.Error)
	}
	sms := body.Find("div", "class", "sms")
	if sms.Error != nil {
		return Message{}, fmt.Errorf("entry has no sms text: %w", sms.Error)
	}

	originator, err := parseOriginator(entry)
	if err != nil {
		return Message{}, err
	}

	createdAt := ""
	right := entry.Find("div", "class", "entry-right")
	if right.Error == nil {
		when := right.Find("span", "class", "muted")
		if when.Error == nil {
			createdAt = strings.TrimSpace(when.FullText())
		}
	}

	return Message{
		Originator: originator,
		Body:       strings.TrimSpace(sms.FullText()),
		CreatedAt:  createdAt,
	}, nil
}

func parseOriginator(entry soup.Root) (string, error) {
	fromLink := entry.Find("a", "class", "from-link")
	if fromLink.Error == nil {
		if text := strings.TrimSpace(fromLink.FullText()); text != "" {
			return text, nil
		}
	}

	left := entry.Find("div", "class", "entry-left")
	if left.Error != nil {
		return "", fmt.Errorf("entry has no originator: %w", left.Error)
	}
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(left.FullText()), "From"))
	if text == "" {
		return "", fmt.Errorf("entry has an empty originator")
	}
	return text, nil
}

// normalizeNumber collapses "+61 420 157 021" into "+61420157021", keeping
// only a leading '+' and digits.
func normalizeNumber(raw string) string {
	var b strings.Builder
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func resolveURL(base, href string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid base URL %q: %w", base, err)
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", fmt.Errorf("invalid href %q: %w", href, err)
	}
	return baseURL.ResolveReference(ref).String(), nil
}
