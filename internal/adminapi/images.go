package adminapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// FileBytesGetter reads a mirrored attachment's bytes from object storage.
type FileBytesGetter interface {
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)
}

// SetFileBytesGetter wires object storage. Without it no image has bytes
// to serve.
func (s *Server) SetFileBytesGetter(g FileBytesGetter) {
	s.mu.Lock()
	s.fileBytes = g
	s.mu.Unlock()
}

var (
	// ErrImageNotFound is an image no Ask conversation was given. One
	// Diderot never stored and one no conversation holds are the same
	// answer.
	ErrImageNotFound = errors.New("image not found")
	// ErrImageNotMirrored is an image whose bytes are not in object
	// storage: not fetched yet, or the deployment has none.
	ErrImageNotMirrored = errors.New("image bytes are not stored")
)

// AskInputImage returns the bytes and media type of a Slack image that an
// Ask conversation was given, for the administrator's page of that
// conversation. An administrator reads every conversation's transcript, so
// the image is not checked against the administrator's own Slack channels:
// the caller is responsible for admitting only administrators. An image no
// conversation holds, or whose message was deleted, is not found.
func (s *Server) AskInputImage(ctx context.Context, sourceImageID string) ([]byte, string, error) {
	pool := s.getStatusPool()
	if pool == nil {
		return nil, "", ErrImageNotFound
	}
	var storageKey, mediaType string
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(ei.storage_key, ''), ei.media_type
		  FROM event_images ei JOIN events e ON e.id = ei.event_id
		 WHERE ei.source_image_id = $1 AND e.source = 'slack' AND e.deleted = false
		   AND EXISTS (SELECT 1 FROM ask_input_images a WHERE a.source_image_id = ei.source_image_id)
		 ORDER BY (ei.storage_key IS NOT NULL AND ei.storage_key <> '') DESC LIMIT 1`,
		sourceImageID).Scan(&storageKey, &mediaType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrImageNotFound
	}
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	getter := s.fileBytes
	s.mu.Unlock()
	if storageKey == "" || getter == nil {
		return nil, "", ErrImageNotMirrored
	}
	data, contentType, err := getter.Get(ctx, storageKey)
	if err != nil {
		return nil, "", err
	}
	if contentType == "" {
		contentType = mediaType
	}
	return data, contentType, nil
}
