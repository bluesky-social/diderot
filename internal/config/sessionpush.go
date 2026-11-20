package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Session ingest sources. Exactly one runs: the Session feed connector's
// change feed (the default) or HTTP push.
const (
	SessionIngestSourceFeed = "session_feed"
	SessionIngestSourcePush = "push"
)

// SessionPushConfig configures PUT /v1/ingest/agent-sessions/{id}. Push is
// off unless DIDEROT_SESSION_INGEST_SOURCE=push.
type SessionPushConfig struct {
	Enabled bool
	// RevisionKey keys revision tokens (DIDEROT_SESSION_PUSH_REVISION_KEY,
	// at least 32 bytes). Rotating it invalidates every writer's token.
	RevisionKey []byte
	// MaxDocumentBytes caps one document after decompression.
	MaxDocumentBytes int64
	// OwnerQuotaBytes / OwnerQuotaRevisions cap each owner's storage.
	OwnerQuotaBytes     int64
	OwnerQuotaRevisions int64
	// SupersededRetention prunes superseded revisions' bytes and records
	// after this long. Zero keeps them for the life of the session.
	SupersededRetention time.Duration
	// Admission bounds.
	MaxConcurrent         int
	MaxConcurrentPerOwner int
	MaxInflightBytes      int64
}

func loadSessionPush(cfg *Config) error {
	source := getenvDefault("DIDEROT_SESSION_INGEST_SOURCE", SessionIngestSourceFeed)
	switch source {
	case SessionIngestSourceFeed:
		return nil
	case SessionIngestSourcePush:
	default:
		return fmt.Errorf("DIDEROT_SESSION_INGEST_SOURCE=%q: want %q or %q", source, SessionIngestSourceFeed, SessionIngestSourcePush)
	}
	p := &cfg.SessionPush
	p.Enabled = true
	p.RevisionKey = []byte(os.Getenv("DIDEROT_SESSION_PUSH_REVISION_KEY"))
	if len(p.RevisionKey) < 32 {
		return fmt.Errorf("DIDEROT_SESSION_PUSH_REVISION_KEY must be at least 32 bytes when push ingest is enabled")
	}
	var err error
	for _, v := range []struct {
		key string
		dst *int64
		def int64
	}{
		{"DIDEROT_SESSION_PUSH_MAX_DOCUMENT_BYTES", &p.MaxDocumentBytes, 64 << 20},
		{"DIDEROT_SESSION_PUSH_OWNER_QUOTA_BYTES", &p.OwnerQuotaBytes, 4 << 30},
		{"DIDEROT_SESSION_PUSH_OWNER_QUOTA_REVISIONS", &p.OwnerQuotaRevisions, 20000},
		{"DIDEROT_SESSION_PUSH_MAX_INFLIGHT_BYTES", &p.MaxInflightBytes, 1 << 30},
	} {
		if *v.dst, err = positiveInt64(v.key, v.def); err != nil {
			return err
		}
	}
	if p.MaxConcurrent, err = getenvPositiveInt("DIDEROT_SESSION_PUSH_MAX_CONCURRENT", 8); err != nil {
		return err
	}
	if p.MaxConcurrentPerOwner, err = getenvPositiveInt("DIDEROT_SESSION_PUSH_MAX_CONCURRENT_PER_OWNER", 2); err != nil {
		return err
	}
	p.SupersededRetention = getenvDuration("DIDEROT_SESSION_PUSH_SUPERSEDED_RETENTION", 0)
	return nil
}

func positiveInt64(key string, def int64) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q must be a positive integer", key, raw)
	}
	return n, nil
}
