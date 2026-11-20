package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bluesky-social/diderot/internal/worker"
)

// Gateway opcodes (https://docs.discord.com/developers/topics/opcodes-and-status-codes).
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// fatalCloseCodes are the gateway close codes after which reconnecting
// cannot help: a bad token, or intents the application may not use.
var fatalCloseCodes = map[int]string{
	4004: "Discord refused the bot token",
	4010: "the gateway shard is invalid",
	4011: "the bot needs sharding, which this connector does not do",
	4012: "the gateway API version is invalid",
	4013: "the gateway intents are invalid",
	4014: "the Message Content intent is not switched on for the bot in Discord's Developer Portal",
}

// FatalGatewayError is a gateway close that reconnecting cannot fix.
type FatalGatewayError struct {
	Code   int
	Reason string
}

func (e *FatalGatewayError) Error() string {
	return fmt.Sprintf("discord gateway closed (%d): %s", e.Code, e.Reason)
}

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// dispatch is one gateway event for the connector.
type dispatch struct {
	Type string
	Data json.RawMessage
}

// gatewayStatus is what the connection reports as it changes.
type gatewayStatus struct {
	Connected bool
	// Resumed says the session resumed, so no events were lost.
	Resumed bool
	Err     error
}

// gateway is one bot session on Discord's gateway: it identifies, resumes
// after a drop, heartbeats, and hands every dispatch to events.
type gateway struct {
	token   string
	intents int
	// url returns where to identify: Discord's gateway URL.
	url    func(ctx context.Context) (string, error)
	logger *slog.Logger
	// events receives each dispatch in order.
	events chan<- dispatch
	// status hears of connects and drops.
	status func(gatewayStatus)
	dialer *websocket.Dialer

	sessionID string
	resumeURL string
	// seq is the last dispatch's sequence number; the heartbeat reads it.
	seq atomic.Int64
}

// run holds the session until ctx ends, or a close code reconnecting cannot
// fix, which it returns as *FatalGatewayError.
func (g *gateway) run(ctx context.Context) error {
	failures := 0
	for ctx.Err() == nil {
		err := g.connect(ctx)
		var fatal *FatalGatewayError
		if errors.As(err, &fatal) {
			g.status(gatewayStatus{Err: err})
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		g.status(gatewayStatus{Err: err})
		failures++
		if err == nil || errors.Is(err, errReconnect) {
			failures = 0
		}
		wait := time.Duration(0)
		if failures > 0 {
			wait = min(time.Duration(1<<min(failures, 6))*time.Second, time.Minute)
		}
		g.logger.Warn("discord gateway disconnected", "err", err, "retry_in", wait, "resumable", g.sessionID != "")
		if err := sleep(ctx, wait+time.Duration(rand.Int64N(int64(time.Second)))); err != nil {
			return nil
		}
	}
	return nil
}

// errReconnect is a drop Discord asked for.
var errReconnect = errors.New("discord asked the bot to reconnect")

// connect runs one connection until it drops.
func (g *gateway) connect(ctx context.Context) error {
	resuming := g.sessionID != "" && g.resumeURL != ""
	base := g.resumeURL
	if !resuming {
		var err error
		if base, err = g.url(ctx); err != nil {
			return fmt.Errorf("gateway URL: %w", err)
		}
	}
	target, err := gatewayTarget(base)
	if err != nil {
		return err
	}
	dialer := g.dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, response, err := dialer.DialContext(ctx, target, http.Header{"User-Agent": {userAgent}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	send := func(op int, d any) error {
		raw, err := json.Marshal(d)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(payload{Op: op, D: raw})
	}
	stop := context.AfterFunc(connCtx, func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = conn.Close()
	})
	defer stop()
	defer func() { _ = conn.Close() }()

	var hello payload
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := conn.ReadJSON(&hello); err != nil {
		return g.closeError(fmt.Errorf("hello: %w", err))
	}
	if hello.Op != opHello {
		return fmt.Errorf("discord gateway: expected hello, got op %d", hello.Op)
	}
	var helloData struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &helloData); err != nil || helloData.HeartbeatInterval <= 0 {
		return errors.New("discord gateway: hello without a heartbeat interval")
	}
	interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond

	if resuming {
		err = send(opResume, map[string]any{"token": g.token, "session_id": g.sessionID, "seq": g.seq.Load()})
	} else {
		err = send(opIdentify, map[string]any{
			"token": g.token, "intents": g.intents,
			"properties": map[string]string{"os": "linux", "browser": "diderot", "device": "diderot"},
		})
	}
	if err != nil {
		return fmt.Errorf("discord gateway: identify: %w", err)
	}

	// Heartbeats: the first after a random part of the interval, then
	// every interval. A beat sent before the last one was acknowledged
	// means the connection is a zombie: drop it and resume.
	var acked sync.Mutex
	awaitingACK := false
	heartbeat := func() error {
		acked.Lock()
		if awaitingACK {
			acked.Unlock()
			return errors.New("discord gateway: heartbeat not acknowledged")
		}
		awaitingACK = true
		acked.Unlock()
		var seq any
		if n := g.seq.Load(); n > 0 {
			seq = n
		}
		return send(opHeartbeat, seq)
	}
	heartbeatErr := make(chan error, 1)
	go func() {
		// A panic here must not crash the whole process over one gateway
		// connection: tear this connection down the same way a heartbeat
		// send failure does (cancel, so run's ReadJSON unblocks and
		// reconnects) instead of leaving it to linger until its read
		// deadline.
		defer worker.Recover(g.logger, "discord-gateway-heartbeat", cancel)
		wait := time.Duration(rand.Float64() * float64(interval))
		for {
			if sleep(connCtx, wait) != nil {
				return
			}
			if err := heartbeat(); err != nil {
				heartbeatErr <- err
				cancel()
				return
			}
			wait = interval
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(interval + 30*time.Second))
		var p payload
		if err := conn.ReadJSON(&p); err != nil {
			select {
			case hbErr := <-heartbeatErr:
				return hbErr
			default:
			}
			if ctx.Err() != nil {
				return nil
			}
			return g.closeError(err)
		}
		if p.S != nil {
			g.seq.Store(*p.S)
		}
		switch p.Op {
		case opDispatch:
			switch p.T {
			case "READY":
				var ready struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
				}
				if err := json.Unmarshal(p.D, &ready); err == nil {
					g.sessionID, g.resumeURL = ready.SessionID, ready.ResumeGatewayURL
				}
				g.status(gatewayStatus{Connected: true})
			case "RESUMED":
				g.status(gatewayStatus{Connected: true, Resumed: true})
			}
			select {
			case g.events <- dispatch{Type: p.T, Data: p.D}:
			case <-ctx.Done():
				return nil
			}
		case opHeartbeat:
			acked.Lock()
			awaitingACK = false
			acked.Unlock()
			if err := heartbeat(); err != nil {
				return err
			}
		case opHeartbeatACK:
			acked.Lock()
			awaitingACK = false
			acked.Unlock()
		case opReconnect:
			return errReconnect
		case opInvalidSession:
			var resumable bool
			_ = json.Unmarshal(p.D, &resumable)
			if !resumable {
				g.forget()
			}
			// Discord asks for a wait of one to five seconds before
			// identifying again.
			if err := sleep(ctx, time.Second+time.Duration(rand.Int64N(int64(4*time.Second)))); err != nil {
				return nil
			}
			return errReconnect
		}
	}
}

// closeError turns a read error into the error connect returns: fatal for a
// close code reconnecting cannot fix. A close that rules out resuming
// forgets the session.
func (g *gateway) closeError(err error) error {
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		return err
	}
	if reason, fatal := fatalCloseCodes[closeErr.Code]; fatal {
		return &FatalGatewayError{Code: closeErr.Code, Reason: reason}
	}
	switch closeErr.Code {
	case 4007, 4009: // invalid sequence, session timed out
		g.forget()
	}
	return err
}

// forget drops the session, so the next connection identifies.
func (g *gateway) forget() {
	g.sessionID, g.resumeURL = "", ""
	g.seq.Store(0)
}

// gatewayTarget adds the API version and encoding to a gateway URL.
func gatewayTarget(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") {
		return "", fmt.Errorf("discord gateway: bad URL %q", base)
	}
	query := u.Query()
	query.Set("v", "10")
	query.Set("encoding", "json")
	u.RawQuery = query.Encode()
	if u.Path == "" {
		u.Path = "/"
	}
	return strings.TrimSpace(u.String()), nil
}
