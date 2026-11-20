package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
)

// What Ask sends to Discord: messages, reactions, threads, typing,
// interaction responses and commands
// (https://docs.discord.com/developers/resources/message,
// https://docs.discord.com/developers/interactions/receiving-and-responding).

// Message flags.
const (
	flagSuppressEmbeds = 1 << 2
	flagEphemeral      = 1 << 6
)

// Interaction types and the response types Ask uses.
const (
	interactionCommand   = 2
	interactionComponent = 3

	responseMessage         = 4
	responseDeferredMessage = 5
	responseDeferredUpdate  = 6
	responseUpdateMessage   = 7
)

// Application command types and contexts.
const (
	commandChatInput = 1
	commandMessage   = 3

	contextGuild = 0
	contextBotDM = 1
)

// Component types.
const (
	componentActionRow = 1
	componentButton    = 2

	buttonSecondary = 2
)

// allowedMentions with nothing in it: a post pings no one, whatever it
// says.
type allowedMentions struct {
	Parse []string `json:"parse"`
}

var noMentions = &allowedMentions{Parse: []string{}}

type component struct {
	Type       int         `json:"type"`
	Style      int         `json:"style,omitempty"`
	Label      string      `json:"label,omitempty"`
	CustomID   string      `json:"custom_id,omitempty"`
	Components []component `json:"components,omitempty"`
}

// buttonRow is one action row holding a button.
func buttonRow(label, customID string) []component {
	return []component{{Type: componentActionRow, Components: []component{
		{Type: componentButton, Style: buttonSecondary, Label: label, CustomID: customID},
	}}}
}

type attachmentRef struct {
	ID          int    `json:"id"`
	Filename    string `json:"filename"`
	Description string `json:"description,omitempty"`
}

// outMessage is a message Ask posts or edits. Components is a pointer so
// that an edit can take every component off (an empty list) or leave them
// (nil).
type outMessage struct {
	Content          string            `json:"content,omitempty"`
	Flags            int               `json:"flags,omitempty"`
	AllowedMentions  *allowedMentions  `json:"allowed_mentions,omitempty"`
	Components       *[]component      `json:"components,omitempty"`
	MessageReference *messageReference `json:"message_reference,omitempty"`
	Attachments      []attachmentRef   `json:"attachments,omitempty"`
}

// upload is a file attached to a post.
type upload struct {
	Name, Description, ContentType string
	Data                           []byte
}

// messageBody is m as a request body: JSON, or with files a multipart form
// whose payload_json is m.
func messageBody(m outMessage, files []upload) (*requestBody, error) {
	if len(files) == 0 {
		return jsonBody(m)
	}
	for i, f := range files {
		m.Attachments = append(m.Attachments, attachmentRef{ID: i, Filename: f.Name, Description: f.Description})
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	part, err := form.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="payload_json"`},
		"Content-Type":        {"application/json"},
	})
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(payload); err != nil {
		return nil, err
	}
	for i, f := range files {
		contentType := f.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		part, err := form.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {fmt.Sprintf(`form-data; name="files[%d]"; filename=%q`, i, f.Name)},
			"Content-Type":        {contentType},
		})
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := form.Close(); err != nil {
		return nil, err
	}
	return &requestBody{contentType: form.FormDataContentType(), data: buf.Bytes()}, nil
}

func (c *Client) createMessage(ctx context.Context, channelID string, m outMessage, files ...upload) (message, error) {
	body, err := messageBody(m, files)
	if err != nil {
		return message{}, err
	}
	var out message
	err = c.do(ctx, http.MethodPost, "/channels/"+channelID+"/messages", body, &out)
	return out, err
}

func (c *Client) editMessage(ctx context.Context, channelID, messageID string, m outMessage) error {
	body, err := jsonBody(m)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, "/channels/"+channelID+"/messages/"+messageID, body, nil)
}

func (c *Client) deleteMessage(ctx context.Context, channelID, messageID string) error {
	return c.do(ctx, http.MethodDelete, "/channels/"+channelID+"/messages/"+messageID, nil, nil)
}

func reactionPath(channelID, messageID, emoji string) string {
	return "/channels/" + channelID + "/messages/" + messageID + "/reactions/" + url.PathEscape(emoji) + "/@me"
}

func (c *Client) addReaction(ctx context.Context, channelID, messageID, emoji string) error {
	return c.do(ctx, http.MethodPut, reactionPath(channelID, messageID, emoji), nil, nil)
}

func (c *Client) removeOwnReaction(ctx context.Context, channelID, messageID, emoji string) error {
	return c.do(ctx, http.MethodDelete, reactionPath(channelID, messageID, emoji), nil, nil)
}

// typing shows the bot typing in a channel for ten seconds, or until it
// posts.
func (c *Client) typing(ctx context.Context, channelID string) error {
	return c.do(ctx, http.MethodPost, "/channels/"+channelID+"/typing", nil, nil)
}

// startThread starts a public thread from a message; the thread's ID is
// the message's.
func (c *Client) startThread(ctx context.Context, channelID, messageID, name string) (channel, error) {
	body, err := jsonBody(map[string]any{"name": name, "auto_archive_duration": 1440})
	if err != nil {
		return channel{}, err
	}
	var out channel
	err = c.do(ctx, http.MethodPost, "/channels/"+channelID+"/messages/"+messageID+"/threads", body, &out)
	return out, err
}

type interactionResponse struct {
	Type int         `json:"type"`
	Data *outMessage `json:"data,omitempty"`
}

// respond is an interaction's first response, due within three seconds.
func (c *Client) respond(ctx context.Context, interactionID, token string, r interactionResponse) error {
	body, err := jsonBody(r)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/interactions/"+interactionID+"/"+token+"/callback", body, nil)
}

// editOriginal edits an interaction's first response, for 15 minutes.
func (c *Client) editOriginal(ctx context.Context, appID, token string, m outMessage, files ...upload) (message, error) {
	body, err := messageBody(m, files)
	if err != nil {
		return message{}, err
	}
	var out message
	err = c.do(ctx, http.MethodPatch, "/webhooks/"+appID+"/"+token+"/messages/@original", body, &out)
	return out, err
}

// followUp adds a message to an interaction's response, for 15 minutes.
func (c *Client) followUp(ctx context.Context, appID, token string, m outMessage, files ...upload) (message, error) {
	body, err := messageBody(m, files)
	if err != nil {
		return message{}, err
	}
	var out message
	err = c.do(ctx, http.MethodPost, "/webhooks/"+appID+"/"+token+"?wait=true", body, &out)
	return out, err
}

type commandOption struct {
	Type        int    `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

type applicationCommand struct {
	Type        int             `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Options     []commandOption `json:"options,omitempty"`
	Contexts    []int           `json:"contexts"`
}

// setGlobalCommands replaces the application's global commands.
func (c *Client) setGlobalCommands(ctx context.Context, appID string, commands []applicationCommand) error {
	body, err := jsonBody(commands)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/applications/"+appID+"/commands", body, nil)
}
