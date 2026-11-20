package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
)

// attachmentReader is the production ask.AttachmentReader: attachments from
// the event store, their bytes from the object storage that mirrors them.
type attachmentReader struct {
	pool *pgxpool.Pool
	// mirror is nil while the deployment has no object storage; no
	// attachment then has a storage key to read.
	mirror files.S3Mirror
}

func (r attachmentReader) Attachment(ctx context.Context, ref events.AttachmentRef, scopes []string) (events.Attachment, error) {
	return events.GetAttachment(ctx, r.pool, ref, scopes)
}

func (r attachmentReader) Bytes(ctx context.Context, storageKey string) ([]byte, error) {
	if r.mirror == nil {
		return nil, errors.New("this deployment has no attachment file storage")
	}
	data, _, err := r.mirror.Get(ctx, storageKey)
	return data, err
}
