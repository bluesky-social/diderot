// Package sessionfixture loads the versioned, offline session bundles the
// session development stack and the session tests share.
package sessionfixture

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SchemaV1         = "diderot.session-fixture.v1"
	SessionMediaType = "application/vnd.bsky.session+ndjson;version=1"
	PrivacyTeam      = "team"
	PrivacyOwnerOnly = "owner_only"
	maxManifestBytes = 1 << 20
	maxStoredBytes   = 16 << 20
	maxExpandedBytes = 64 << 20
	maxSessions      = 1000
	maxTitleBytes    = 200
)

var tokenPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type Manifest struct {
	SchemaVersion   string          `json:"schema_version"`
	CapturedAt      string          `json:"captured_at"`
	SourceRevisions SourceRevisions `json:"source_revisions"`
	AllowedPersonas []string        `json:"allowed_personas"`
	Sessions        []Session       `json:"sessions"`
}

type SourceRevisions struct {
	Diderot string `json:"diderot"`
}

type Session struct {
	SessionID     string        `json:"session_id"`
	Owner         string        `json:"owner"`
	Client        string        `json:"client"`
	Harness       string        `json:"harness"`
	OwnerKind     string        `json:"owner_kind"`
	SessionRole   string        `json:"session_role"`
	OriginApp     string        `json:"origin_app"`
	Revision      string        `json:"revision"`
	RewriteEpoch  int64         `json:"rewrite_epoch"`
	Complete      bool          `json:"complete"`
	StartedAt     string        `json:"started_at"`
	UpdatedAt     string        `json:"updated_at"`
	ChangeSeq     int64         `json:"change_seq"`
	RecordCount   int           `json:"record_count"`
	UserTurnCount int           `json:"user_turn_count"`
	ContentBytes  int64         `json:"content_bytes"`
	Title         string        `json:"title"`
	Privacy       string        `json:"privacy"`
	Document      Object        `json:"document"`
	Source        *SourceObject `json:"source,omitempty"`
}

type Object struct {
	Path               string `json:"path"`
	MediaType          string `json:"media_type"`
	Revision           string `json:"revision"`
	StoredSHA256       string `json:"stored_sha256"`
	UncompressedSHA256 string `json:"uncompressed_sha256"`
}

type SourceObject struct {
	Path               string `json:"path"`
	Harness            string `json:"harness"`
	MediaType          string `json:"media_type"`
	StoredSHA256       string `json:"stored_sha256"`
	UncompressedSHA256 string `json:"uncompressed_sha256"`
	Redacted           bool   `json:"redacted"`
}

// Bundle retains validated, uncompressed content keyed by session UUID.
type Bundle struct {
	Manifest  Manifest
	Documents map[string][]byte
	Sources   map[string][]byte
	root      string
}

// Load validates a complete bundle before returning any of its content.
func Load(root string) (*Bundle, error) {
	if err := validateRoot(root); err != nil {
		return nil, err
	}
	manifestBytes, err := readRegular(root, "manifest.json", maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("session fixture manifest: %w", err)
	}
	var manifest Manifest
	if err := decodeExact(manifestBytes, &manifest, true); err != nil {
		return nil, fmt.Errorf("session fixture manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return nil, fmt.Errorf("session fixture manifest: %w", err)
	}
	if err := validateFileSet(root, manifest); err != nil {
		return nil, fmt.Errorf("session fixture files: %w", err)
	}

	bundle := &Bundle{
		Manifest:  manifest,
		Documents: make(map[string][]byte, len(manifest.Sessions)),
		Sources:   make(map[string][]byte),
		root:      root,
	}
	for i := range manifest.Sessions {
		session := &manifest.Sessions[i]
		doc, err := loadObject(root, session.Document.Path, session.Document.StoredSHA256, session.Document.UncompressedSHA256)
		if err != nil {
			return nil, fmt.Errorf("session %s document: %w", session.SessionID, err)
		}
		if err := validateDocument(*session, doc); err != nil {
			return nil, fmt.Errorf("session %s document: %w", session.SessionID, err)
		}
		bundle.Documents[session.SessionID] = doc
		if session.Source != nil {
			source, err := loadObject(root, session.Source.Path, session.Source.StoredSHA256, session.Source.UncompressedSHA256)
			if err != nil {
				return nil, fmt.Errorf("session %s source: %w", session.SessionID, err)
			}
			bundle.Sources[session.SessionID] = source
		}
	}
	return bundle, nil
}

// ReadStoredDocument re-reads and re-hashes the compressed object, so a
// caller that serves documents can check each request rather than trust the
// one-time import validation.
func (b *Bundle) ReadStoredDocument(sessionID string) ([]byte, error) {
	if b == nil || b.root == "" {
		return nil, errors.New("session fixture bundle is not loaded")
	}
	for _, session := range b.Manifest.Sessions {
		if session.SessionID != sessionID {
			continue
		}
		stored, err := readRegular(b.root, session.Document.Path, maxStoredBytes)
		if err != nil {
			return nil, err
		}
		if digest(stored) != session.Document.StoredSHA256 {
			return nil, errors.New("stored_sha256 mismatch")
		}
		return stored, nil
	}
	return nil, fmt.Errorf("session document %q is not in the bundle", sessionID)
}

func validateRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("open bundle root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("bundle root must be a directory, not a symlink")
	}
	return nil
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaV1 {
		return fmt.Errorf("unsupported schema_version %q", manifest.SchemaVersion)
	}
	if _, err := time.Parse(time.RFC3339, manifest.CapturedAt); err != nil {
		return fmt.Errorf("captured_at must be RFC3339: %w", err)
	}
	if !isCanonicalValue(manifest.SourceRevisions.Diderot) {
		return errors.New("the diderot source revision is required")
	}
	if len(manifest.AllowedPersonas) == 0 {
		return errors.New("allowed_personas must not be empty")
	}
	personas := make(map[string]struct{}, len(manifest.AllowedPersonas))
	for _, persona := range manifest.AllowedPersonas {
		if !isCanonicalOwner(persona) {
			return fmt.Errorf("allowed persona %q is not canonical", persona)
		}
		if _, exists := personas[persona]; exists {
			return fmt.Errorf("duplicate allowed persona %q", persona)
		}
		personas[persona] = struct{}{}
	}
	if len(manifest.Sessions) == 0 || len(manifest.Sessions) > maxSessions {
		return fmt.Errorf("sessions must contain between 1 and %d entries", maxSessions)
	}
	seenIDs := make(map[string]struct{}, len(manifest.Sessions))
	seenPaths := make(map[string]struct{}, len(manifest.Sessions)*2)
	for i := range manifest.Sessions {
		session := &manifest.Sessions[i]
		if err := validateSession(*session, personas); err != nil {
			return fmt.Errorf("sessions[%d]: %w", i, err)
		}
		if _, exists := seenIDs[session.SessionID]; exists {
			return fmt.Errorf("duplicate session_id %q", session.SessionID)
		}
		seenIDs[session.SessionID] = struct{}{}
		for _, path := range sessionPaths(*session) {
			if _, exists := seenPaths[path]; exists {
				return fmt.Errorf("duplicate object path %q", path)
			}
			seenPaths[path] = struct{}{}
		}
	}
	return nil
}

func validateSession(session Session, personas map[string]struct{}) error {
	parsed, err := uuid.Parse(session.SessionID)
	if err != nil || parsed.String() != session.SessionID {
		return fmt.Errorf("session_id %q is not a canonical UUID", session.SessionID)
	}
	if !isCanonicalOwner(session.Owner) {
		return fmt.Errorf("owner %q is not canonical", session.Owner)
	}
	if _, ok := personas[session.Owner]; !ok {
		return fmt.Errorf("owner %q is absent from allowed_personas", session.Owner)
	}
	for name, value := range map[string]string{"client": session.Client, "harness": session.Harness, "owner_kind": session.OwnerKind} {
		if !tokenPattern.MatchString(value) {
			return fmt.Errorf("%s %q is not canonical", name, value)
		}
	}
	if session.OriginApp != "" && !tokenPattern.MatchString(session.OriginApp) {
		return fmt.Errorf("origin_app %q is not canonical", session.OriginApp)
	}
	if session.SessionRole != "primary" && session.SessionRole != "secondary" {
		return fmt.Errorf("unknown session_role %q", session.SessionRole)
	}
	if !validRevision(session.Revision) {
		return fmt.Errorf("revision %q is not a sha256 revision", session.Revision)
	}
	if session.RewriteEpoch < 0 || session.ChangeSeq <= 0 || session.RecordCount < 0 || session.UserTurnCount < 0 || session.ContentBytes < 0 {
		return errors.New("revision epochs, sequence, and counts must be non-negative")
	}
	started, err := time.Parse(time.RFC3339, session.StartedAt)
	if err != nil {
		return fmt.Errorf("started_at must be RFC3339: %w", err)
	}
	updated, err := time.Parse(time.RFC3339, session.UpdatedAt)
	if err != nil || updated.Before(started) {
		return errors.New("updated_at must be RFC3339 and not precede started_at")
	}
	if strings.TrimSpace(session.Title) != session.Title || session.Title == "" || len(session.Title) > maxTitleBytes {
		return fmt.Errorf("title must contain between 1 and %d canonical bytes", maxTitleBytes)
	}
	if session.Privacy != PrivacyTeam && session.Privacy != PrivacyOwnerOnly {
		return fmt.Errorf("unknown privacy %q", session.Privacy)
	}
	if err := validateObject(session.Document, "documents/"); err != nil {
		return fmt.Errorf("document: %w", err)
	}
	if session.Document.MediaType != SessionMediaType {
		return fmt.Errorf("document media_type %q is unsupported", session.Document.MediaType)
	}
	if session.Document.Revision != session.Revision {
		return errors.New("document revision does not match session revision")
	}
	if session.Source != nil {
		if !session.Source.Redacted {
			return errors.New("source fixture is not marked redacted")
		}
		if session.Source.Harness != session.Harness {
			return errors.New("source harness does not match session harness")
		}
		if !isCanonicalValue(session.Source.MediaType) {
			return errors.New("source media_type is required")
		}
		if err := validateObject(Object{Path: session.Source.Path, StoredSHA256: session.Source.StoredSHA256, UncompressedSHA256: session.Source.UncompressedSHA256}, "sources/"); err != nil {
			return fmt.Errorf("source: %w", err)
		}
	}
	return nil
}

func validateObject(object Object, prefix string) error {
	if err := validateBundlePath(object.Path, prefix); err != nil {
		return err
	}
	if !validDigest(object.StoredSHA256) || !validDigest(object.UncompressedSHA256) {
		return errors.New("stored_sha256 and uncompressed_sha256 must be lowercase SHA-256 digests")
	}
	return nil
}

func validateBundlePath(path, prefix string) error {
	if path == "" || strings.Contains(path, `\`) || filepath.IsAbs(path) || filepath.Clean(path) != filepath.FromSlash(path) || !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, ".gz") {
		return fmt.Errorf("path %q is not a confined gzip path under %s", path, prefix)
	}
	return nil
}

func loadObject(root, path, storedDigest, expandedDigest string) ([]byte, error) {
	stored, err := readRegular(root, path, maxStoredBytes)
	if err != nil {
		return nil, err
	}
	if digest(stored) != storedDigest {
		return nil, errors.New("stored_sha256 mismatch")
	}
	reader, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	expanded, readErr := io.ReadAll(io.LimitReader(reader, maxExpandedBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("decompress: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close gzip: %w", closeErr)
	}
	if len(expanded) > maxExpandedBytes {
		return nil, fmt.Errorf("uncompressed object exceeds %d bytes", maxExpandedBytes)
	}
	if digest(expanded) != expandedDigest {
		return nil, errors.New("uncompressed_sha256 mismatch")
	}
	return expanded, nil
}

func readRegular(root, path string, limit int64) ([]byte, error) {
	parts := strings.Split(filepath.FromSlash(path), string(filepath.Separator))
	current := root
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("path %q contains a symlink", path)
		}
	}
	info, err := os.Stat(current)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path %q is not a regular file", path)
	}
	file, err := os.Open(current)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("path %q exceeds %d bytes", path, limit)
	}
	return data, nil
}

func validateFileSet(root string, manifest Manifest) error {
	referenced := map[string]struct{}{"manifest.json": {}}
	allowedDirectories := map[string]struct{}{".": {}}
	for _, session := range manifest.Sessions {
		for _, path := range sessionPaths(session) {
			referenced[path] = struct{}{}
			allowedDirectories[filepath.ToSlash(filepath.Dir(path))] = struct{}{}
		}
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q is a symlink", relative)
		}
		if entry.IsDir() {
			if _, ok := allowedDirectories[relative]; !ok {
				return fmt.Errorf("unexpected directory %q", relative)
			}
			return nil
		}
		if _, ok := referenced[relative]; !ok {
			return fmt.Errorf("unreferenced file %q", relative)
		}
		return nil
	})
}

type documentEnvelope struct {
	Type          string `json:"type"`
	Schema        string `json:"schema"`
	SessionID     string `json:"session_id"`
	Source        string `json:"source"`
	Owner         string `json:"owner"`
	OwnerKind     string `json:"owner_kind"`
	SessionRole   string `json:"session_role"`
	OriginApp     string `json:"origin_app"`
	Revision      string `json:"revision"`
	RewriteEpoch  int64  `json:"rewrite_epoch"`
	RecordCount   int    `json:"record_count"`
	UserTurnCount int    `json:"user_turn_count"`
	ContentBytes  int64  `json:"content_bytes"`
	StartedAt     string `json:"started_at"`
	UpdatedAt     string `json:"updated_at"`
	Complete      bool   `json:"complete"`
}

type documentRecord struct {
	Index     int             `json:"index"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content,omitempty"`
	Timestamp string          `json:"timestamp"`
}

func validateDocument(session Session, data []byte) error {
	var envelope documentEnvelope
	foundEnvelope := false
	recordCount := 0
	userTurns := 0
	var contentBytes int64
	for _, rawLine := range bytes.Split(data, []byte{'\n'}) {
		line := bytes.TrimSpace(rawLine)
		if len(line) == 0 {
			continue
		}
		if !foundEnvelope {
			if err := decodeExact(line, &envelope, false); err != nil {
				return fmt.Errorf("decode envelope: %w", err)
			}
			foundEnvelope = true
			continue
		}
		var record documentRecord
		if err := decodeExact(line, &record, false); err != nil {
			return fmt.Errorf("decode record %d: %w", recordCount, err)
		}
		if record.Index != recordCount || strings.TrimSpace(record.ID) == "" {
			return fmt.Errorf("record %d has invalid index or id", recordCount)
		}
		switch record.Role {
		case "user":
			userTurns++
		case "assistant", "reasoning", "tool", "system":
		default:
			return fmt.Errorf("record %d has unsupported role %q", recordCount, record.Role)
		}
		if _, err := time.Parse(time.RFC3339, record.Timestamp); err != nil {
			return fmt.Errorf("record %d timestamp must be RFC3339", recordCount)
		}
		contentBytes += int64(len(line) + 1)
		recordCount++
	}
	if !foundEnvelope {
		return errors.New("document is empty")
	}
	if envelope.Type != "session" || envelope.Schema != "bsky.session.v1" {
		return fmt.Errorf("unsupported envelope type=%q schema=%q", envelope.Type, envelope.Schema)
	}
	if envelope.SessionID != session.SessionID || envelope.Owner != session.Owner || envelope.Source != session.Client || envelope.OwnerKind != session.OwnerKind || envelope.SessionRole != session.SessionRole || envelope.OriginApp != session.OriginApp || envelope.Revision != session.Revision || envelope.RewriteEpoch != session.RewriteEpoch || envelope.Complete != session.Complete || envelope.StartedAt != session.StartedAt || envelope.UpdatedAt != session.UpdatedAt {
		return errors.New("envelope identity or revision metadata does not match manifest")
	}
	if envelope.RecordCount != recordCount || envelope.UserTurnCount != userTurns || envelope.ContentBytes != contentBytes {
		return errors.New("envelope counts do not match document records")
	}
	if session.RecordCount != recordCount || session.UserTurnCount != userTurns || session.ContentBytes != contentBytes {
		return errors.New("manifest counts do not match document records")
	}
	return nil
}

func decodeExact(data []byte, dst any, disallowUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func validRevision(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validDigest(strings.TrimPrefix(value, "sha256:"))
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isCanonicalOwner(value string) bool {
	return isCanonicalValue(value) && strings.ToLower(value) == value
}

func isCanonicalValue(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func sessionPaths(session Session) []string {
	paths := []string{session.Document.Path}
	if session.Source != nil {
		paths = append(paths, session.Source.Path)
	}
	return paths
}
