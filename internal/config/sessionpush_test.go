package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadSessionPush(t *testing.T) {
	t.Run("default is the session feed", func(t *testing.T) {
		var cfg Config
		if err := loadSessionPush(&cfg); err != nil || cfg.SessionPush.Enabled {
			t.Fatalf("enabled=%v err=%v", cfg.SessionPush.Enabled, err)
		}
	})
	t.Run("session feed", func(t *testing.T) {
		t.Setenv("DIDEROT_SESSION_INGEST_SOURCE", "session_feed")
		var cfg Config
		if err := loadSessionPush(&cfg); err != nil || cfg.SessionPush.Enabled {
			t.Fatalf("enabled=%v err=%v", cfg.SessionPush.Enabled, err)
		}
	})
	t.Run("unknown source", func(t *testing.T) {
		for _, source := range []string{"both", "feed"} {
			t.Setenv("DIDEROT_SESSION_INGEST_SOURCE", source)
			err := loadSessionPush(&Config{})
			if err == nil || !strings.Contains(err.Error(), `want "session_feed" or "push"`) {
				t.Fatalf("%s: err = %v", source, err)
			}
		}
	})
	t.Run("push needs a key", func(t *testing.T) {
		t.Setenv("DIDEROT_SESSION_INGEST_SOURCE", "push")
		t.Setenv("DIDEROT_SESSION_PUSH_REVISION_KEY", "short")
		if err := loadSessionPush(&Config{}); err == nil {
			t.Fatal("short key accepted")
		}
	})
	t.Run("push defaults", func(t *testing.T) {
		t.Setenv("DIDEROT_SESSION_INGEST_SOURCE", "push")
		t.Setenv("DIDEROT_SESSION_PUSH_REVISION_KEY", "test-revision-key-test-revision-key")
		t.Setenv("DIDEROT_SESSION_PUSH_SUPERSEDED_RETENTION", "720h")
		var cfg Config
		if err := loadSessionPush(&cfg); err != nil {
			t.Fatal(err)
		}
		p := cfg.SessionPush
		if !p.Enabled || p.MaxDocumentBytes != 64<<20 || p.OwnerQuotaBytes != 4<<30 || p.OwnerQuotaRevisions != 20000 ||
			p.MaxConcurrent != 8 || p.MaxConcurrentPerOwner != 2 || p.MaxInflightBytes != 1<<30 || p.SupersededRetention != 720*time.Hour {
			t.Fatalf("config = %+v", p)
		}
	})
	t.Run("bad limit", func(t *testing.T) {
		t.Setenv("DIDEROT_SESSION_INGEST_SOURCE", "push")
		t.Setenv("DIDEROT_SESSION_PUSH_REVISION_KEY", "test-revision-key-test-revision-key")
		t.Setenv("DIDEROT_SESSION_PUSH_OWNER_QUOTA_BYTES", "0")
		if err := loadSessionPush(&Config{}); err == nil {
			t.Fatal("zero quota accepted")
		}
	})
}
