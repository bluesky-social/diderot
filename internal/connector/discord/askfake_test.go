package discord

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The fake Discord's write side, for Ask: messages, reactions, threads,
// typing, interaction responses and commands. Every write is recorded.

type fakeAsk struct {
	writes []fakeWrite
	nextID int
	// originals are interactions' first responses, by token.
	originals map[string]*message
	commands  []applicationCommand
	// members answer a member read instead of a plain member, by user;
	// guildFeatures are the guild's features.
	members       map[string]member
	guildFeatures []string
}

// fakeWrite is one write as the fake received it.
type fakeWrite struct {
	Method, Path string
	Message      outMessage
	Response     interactionResponse
	Files        []string
}

// newMessageID is a fresh snowflake for something the bot posts.
func (f *fakeDiscord) newMessageID() string {
	f.ask.nextID++
	return fakeID(time.Now().UTC(), 1000+f.ask.nextID)
}

// readWrite decodes a write's body: JSON, or a multipart form whose
// payload_json is the message, with its files' names.
func readWrite(r *http.Request, into any) []string {
	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(mediaType, "multipart/") {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, into)
		return nil
	}
	var files []string
	form := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := form.NextPart()
		if err != nil {
			return files
		}
		data, _ := io.ReadAll(part)
		if part.FormName() == "payload_json" {
			_ = json.Unmarshal(data, into)
		} else {
			files = append(files, part.FileName())
		}
	}
}

// serveWrite answers a write; f.mu is held.
func (f *fakeDiscord) serveWrite(w http.ResponseWriter, r *http.Request, path string, parts []string) {
	rec := fakeWrite{Method: r.Method, Path: path}
	switch {
	case len(parts) == 4 && parts[0] == "interactions" && parts[3] == "callback":
		readWrite(r, &rec.Response)
		f.ask.writes = append(f.ask.writes, rec)
		w.WriteHeader(http.StatusNoContent)
		return
	case parts[0] == "webhooks":
		rec.Files = readWrite(r, &rec.Message)
		f.ask.writes = append(f.ask.writes, rec)
		token := parts[2]
		if f.ask.originals == nil {
			f.ask.originals = map[string]*message{}
		}
		if len(parts) == 5 && parts[4] == "@original" {
			original := f.ask.originals[token]
			if original == nil {
				original = &message{ID: f.newMessageID(), Author: user{ID: fakeBot, Bot: true}}
				f.ask.originals[token] = original
			}
			if rec.Message.Content != "" {
				original.Content = rec.Message.Content
			}
			if rec.Message.Components != nil {
				original.Components = *rec.Message.Components
			}
			f.writeJSON(w, original)
			return
		}
		f.writeJSON(w, message{ID: f.newMessageID(), Content: rec.Message.Content, Author: user{ID: fakeBot, Bot: true}})
		return
	case len(parts) == 3 && parts[0] == "applications" && parts[2] == "commands":
		readWrite(r, &f.ask.commands)
		f.ask.writes = append(f.ask.writes, rec)
		f.writeJSON(w, f.ask.commands)
		return
	}
	if r.Method == http.MethodPost && len(parts) == 5 && parts[0] == "channels" && parts[4] == "threads" {
		var body struct {
			Name string `json:"name"`
		}
		readWrite(r, &body)
		rec.Message.Content = body.Name
		f.ask.writes = append(f.ask.writes, rec)
		thread := channel{ID: parts[3], Type: channelPublicThread, GuildID: fakeGuild, ParentID: parts[1], Name: body.Name}
		f.threads = append(f.threads, thread)
		f.writeJSON(w, thread)
		return
	}
	rec.Files = readWrite(r, &rec.Message)
	f.ask.writes = append(f.ask.writes, rec)
	switch {
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages":
		m := message{ID: f.newMessageID(), ChannelID: parts[1], Author: user{ID: fakeBot, Username: "diderot-test", Bot: true},
			Content: rec.Message.Content, Timestamp: time.Now().UTC()}
		if rec.Message.Components != nil {
			m.Components = *rec.Message.Components
		}
		for i, name := range rec.Files {
			m.Attachments = append(m.Attachments, attachment{ID: fakeID(time.Now().UTC(), 3000+f.ask.nextID+i), Filename: name})
		}
		f.messages[parts[1]] = append(f.messages[parts[1]], m)
		f.writeJSON(w, m)
	case r.Method == http.MethodPatch && len(parts) == 4 && parts[0] == "channels" && parts[2] == "messages":
		for i := range f.messages[parts[1]] {
			if m := &f.messages[parts[1]][i]; m.ID == parts[3] {
				if rec.Message.Content != "" {
					m.Content = rec.Message.Content
				}
				if rec.Message.Components != nil {
					m.Components = *rec.Message.Components
				}
				f.writeJSON(w, m)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		f.writeJSON(w, map[string]any{"code": 10008})
	case r.Method == http.MethodDelete && len(parts) == 4 && parts[0] == "channels" && parts[2] == "messages":
		before := len(f.messages[parts[1]])
		f.messages[parts[1]] = slices.DeleteFunc(f.messages[parts[1]], func(m message) bool { return m.ID == parts[3] })
		if len(f.messages[parts[1]]) == before {
			w.WriteHeader(http.StatusNotFound)
			f.writeJSON(w, map[string]any{"code": 10008})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		// Reactions and typing.
		w.WriteHeader(http.StatusNoContent)
	}
}

// writes are the writes so far whose path starts with prefix and whose
// method is method ("" for any).
func (f *fakeDiscord) writes(method, prefix string) []fakeWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeWrite
	for _, w := range f.ask.writes {
		if (method == "" || w.Method == method) && strings.HasPrefix(w.Path, prefix) {
			out = append(out, w)
		}
	}
	return out
}

// channelMessages are a channel's messages, oldest first.
func (f *fakeDiscord) channelMessages(channelID string) []message {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := slices.Clone(f.messages[channelID])
	slices.SortFunc(all, func(a, b message) int {
		if laterID(a.ID, b.ID) {
			return 1
		}
		return -1
	})
	return all
}
