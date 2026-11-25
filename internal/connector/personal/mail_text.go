package personal

import (
	"bytes"
	"io"
	"mime"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"

	"github.com/bluesky-social/diderot/internal/events"
)

func mailEvent(msg *imapclient.FetchMessageBuffer, section *imap.FetchItemBodySection, id, scope string) events.Event {
	event := events.Event{Source: imapName, ExternalID: id, Scope: scope, OccurredAt: msg.InternalDate.UTC(), Title: "(No subject)"}
	var lines []string
	if env := msg.Envelope; env != nil {
		if !env.Date.IsZero() {
			event.OccurredAt = env.Date.UTC()
		}
		if subject := cleanText(env.Subject); subject != "" {
			event.Title = subject
		}
		for _, from := range env.From {
			if event.ActorID = cleanText(from.Addr()); event.ActorID != "" {
				event.ActorCanonical = strings.ToLower(event.ActorID)
				event.Actor = cleanText(from.Name)
				if event.Actor == "" {
					event.Actor = event.ActorID
				}
				break
			}
		}
		lines = appendAddresses(lines, "From", env.From)
		lines = appendAddresses(lines, "To", env.To)
		lines = appendAddresses(lines, "Cc", env.Cc)
	}
	lines = append(lines, "Subject: "+event.Title)
	if text := messageText(msg.FindBodySection(section)); text != "" {
		lines = append(lines, "", text)
	}
	if msg.RFC822Size > maxFetchBytes {
		lines = append(lines, "", "[Only the beginning of this message was downloaded.]")
	}
	event.Body = strings.Join(lines, "\n")
	return event
}

func appendAddresses(lines []string, label string, addresses []imap.Address) []string {
	var formatted []string
	for _, address := range addresses {
		addr := cleanText(address.Addr())
		if addr == "" {
			continue
		}
		if name := cleanText(address.Name); name != "" && name != addr {
			addr = name + " <" + addr + ">"
		}
		formatted = append(formatted, addr)
	}
	if len(formatted) > 0 {
		lines = append(lines, label+": "+strings.Join(formatted, ", "))
	}
	return lines
}

// messageText reads decoded inline text. The FETCH byte cap also bounds
// inputs here, and the bytes read before a truncated MIME part remain useful.
func messageText(raw []byte) string {
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return ""
	}
	var plain, rich []string
	for {
		part, err := r.NextPart()
		if err != nil {
			if part != nil && message.IsUnknownCharset(err) {
				continue
			}
			break
		}
		if _, ok := part.Header.(*mail.InlineHeader); !ok {
			continue
		}
		mediaType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if mediaType == "" && part.Header.Get("Content-Type") == "" {
			mediaType = "text/plain"
		} else if err != nil {
			continue
		}
		switch mediaType {
		case "text/plain":
			data, _ := io.ReadAll(part.Body)
			if text := cleanText(string(data)); text != "" {
				plain = append(plain, text)
			}
		case "text/html":
			if text := htmlText(part.Body); text != "" {
				rich = append(rich, text)
			}
		}
	}
	if len(plain) > 0 {
		return strings.Join(plain, "\n\n")
	}
	return strings.Join(rich, "\n\n")
}

func htmlText(r io.Reader) string {
	tokens := html.NewTokenizer(r)
	var text strings.Builder
	var hidden string
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return cleanText(text.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			tag, _ := tokens.TagName()
			switch string(tag) {
			case "head", "script", "style":
				if hidden == "" {
					hidden = string(tag)
				}
			case "br":
				if hidden == "" {
					text.WriteByte('\n')
				}
			}
		case html.EndTagToken:
			tag, _ := tokens.TagName()
			if string(tag) == hidden {
				hidden = ""
			}
			switch string(tag) {
			case "p", "div", "li", "tr", "blockquote":
				if hidden == "" {
					text.WriteByte('\n')
				}
			}
		case html.TextToken:
			if hidden == "" {
				text.Write(tokens.Text())
			}
		}
	}
}
