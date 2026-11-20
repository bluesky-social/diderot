package sessiondev

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"strings"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VectorDim is the session-dev collection's vector size.
const VectorDim = 1024

// DeterministicEmbedder provides stable local vectors without constructing an
// inference client. Equal text produces equal normalized vectors.
type DeterministicEmbedder struct{}

func (DeterministicEmbedder) ID() string      { return "session-dev:deterministic:v1" }
func (DeterministicEmbedder) Dimensions() int { return VectorDim }

func (DeterministicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for index, text := range texts {
		digest := sha256.Sum256([]byte(text))
		vector := make([]float32, VectorDim)
		var norm float64
		for dimension := range vector {
			value := float32(int(digest[dimension%len(digest)])-127) / 128
			vector[dimension] = value
			norm += float64(value * value)
		}
		scale := float32(1 / math.Sqrt(norm))
		for dimension := range vector {
			vector[dimension] *= scale
		}
		vectors[index] = vector
	}
	return vectors, nil
}

// SeedQdrant writes the fixture's current event chunks through the production
// point shape. The caller owns collection reset and loopback validation.
func SeedQdrant(ctx context.Context, pool *pgxpool.Pool, store *qdrantstore.Store, embedder DeterministicEmbedder) error {
	rows, err := pool.Query(ctx, `
		SELECT chunk.id, chunk.event_id, chunk.content, event.source,
		       COALESCE(event.actor_id, ''), COALESCE(event.actor_canonical, ''),
		       COALESCE(event.thread_id, ''), chunk.occurred_at, event.scope
		  FROM event_chunks chunk
		  JOIN events event ON event.id = chunk.event_id
		 WHERE event.source = 'agent_session' AND event.deleted = false
		 ORDER BY chunk.id
	`)
	if err != nil {
		return fmt.Errorf("read session-dev chunks for Qdrant: %w", err)
	}
	defer rows.Close()
	type row struct {
		point qdrantstore.Point
		text  string
	}
	var fixtureRows []row
	for rows.Next() {
		var item row
		var pointID int64
		if err := rows.Scan(&pointID, &item.point.EventID, &item.text, &item.point.Source,
			&item.point.ActorID, &item.point.ActorCanonical, &item.point.ThreadID,
			&item.point.OccurredAt, &item.point.Scope); err != nil {
			return err
		}
		if pointID <= 0 {
			return fmt.Errorf("invalid session-dev chunk id %d", pointID)
		}
		item.point.ID = uint64(pointID)
		fixtureRows = append(fixtureRows, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	texts := make([]string, len(fixtureRows))
	for index := range fixtureRows {
		texts[index] = fixtureRows[index].text
	}
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	points := make([]qdrantstore.Point, len(fixtureRows))
	for index := range fixtureRows {
		fixtureRows[index].point.Vector = vectors[index]
		points[index] = fixtureRows[index].point
	}
	return store.Upsert(ctx, points)
}

// ScopeResolver gives a local persona the ordinary team scopes plus only its
// own private session scope.
type ScopeResolver struct{}

func (ScopeResolver) ScopesForEmail(_ context.Context, email string) []string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return []string{events.ScopePublic, events.ScopeInternal}
	}
	return []string{events.ScopePublic, events.ScopeInternal, events.ScopeOwner(email)}
}
