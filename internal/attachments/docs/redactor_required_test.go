package docs

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

type openGate struct{}

func (openGate) DocIngestEnabled(context.Context, string) (bool, error) { return true, nil }

func TestWorkerRunRefusesWithoutARedactor(t *testing.T) {
	errBuild := errors.New("detector did not build")
	original := newRedactor
	newRedactor = func() (files.Redactor, error) { return nil, errBuild }
	t.Cleanup(func() { newRedactor = original })

	// Every other requirement is met, and the context is already done, so
	// a Run that got past the redactor would return nil.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker := &Worker{
		Pool:     &pgxpool.Pool{},
		S3:       &fakeS3{},
		Router:   &Router{},
		Fetchers: map[string]files.Fetcher{"slack": nil},
		Gate:     openGate{},
	}
	if err := worker.Run(ctx); !errors.Is(err, errBuild) {
		t.Fatalf("Run = %v, want the redactor's construction error", err)
	}
}
