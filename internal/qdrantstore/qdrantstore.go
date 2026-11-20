// Package qdrantstore is diderot's thin wrapper around the official
// github.com/qdrant/go-client. It fixes our collection shape (cosine,
// scalar-quantized, in-memory HNSW, payload indexes on the fields /v1/ask
// filters on) so the rest of the codebase doesn't have to carry qdrant
// construction boilerplate. The vector size comes from the configured
// embedding dimension.
//
// Scope: one embedder, one collection. Another embedder gets its own
// Store instance pointed at its own collection. We do not fan out
// multiple embedders through a single Store.
package qdrantstore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Config is the subset of qdrant client options diderot sets. Anything
// not exposed here is deliberately fixed: we do not want per-caller
// distance metric or dimension drift.
type Config struct {
	// Addr is a "host:port" pair. The gRPC port on qdrant is 6334;
	// DIDEROT_QDRANT_ADDR should encode both.
	Addr string
	// APIKey is optional. Empty string disables the auth interceptor.
	APIKey string
	// UseTLS dials Qdrant over TLS 1.3 and verifies its certificate against
	// RootCAs, or against the system's CAs when RootCAs is nil.
	UseTLS  bool
	RootCAs *x509.CertPool
	// Collection is the target collection name, e.g.
	// "diderot_chunks_local_embed_1024". Embedder-version suffix is the
	// versioning story: renaming a collection = new suffix.
	Collection string
	// Dimensions is the vector size. Collection
	// creation pins it; changing it later means a new collection under a
	// new name. Zero is allowed for payload-only tools, which then cannot
	// create, validate, write, or search vectors.
	Dimensions uint64
	// ShardNumber and ReplicationFactor describe the collection topology.
	// Zero defaults each value to one for single-node development Qdrant.
	// A deployment sets these explicitly because topology is an operational
	// choice, not something the client should infer from peer count.
	ShardNumber       uint32
	ReplicationFactor uint32
}

// ParseAddr splits a "host:port" string the way DIDEROT_QDRANT_ADDR
// expects. Delegates to net.SplitHostPort so IPv6 bracket rules are
// handled correctly — a bare "::1" (no brackets, no port) is rejected
// rather than parsed as host="::" port="1".
func ParseAddr(addr string) (host string, port int, err error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("qdrant addr %q: %w", addr, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("qdrant addr %q: missing host", addr)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("qdrant addr %q: port %q not numeric", addr, portStr)
	}
	return host, port, nil
}

// Store is a configured handle to one collection on a qdrant cluster.
// Safe for concurrent use by many goroutines — the underlying
// qdrant.Client is already goroutine-safe.
type Store struct {
	cli               *qdrant.Client
	addr              string
	tlsConfig         *tls.Config // nil when Qdrant is dialed without TLS
	collection        string
	dimensions        uint64
	shardNumber       uint32
	replicationFactor uint32
	metrics           *metrics
}

// New dials qdrant with diderot's fixed settings. Does not touch the
// collection; call EnsureCollection to create / validate it.
func New(cfg Config, reg prometheus.Registerer) (*Store, error) {
	if cfg.Collection == "" {
		return nil, errors.New("qdrantstore: Config.Collection is required")
	}
	host, port, err := ParseAddr(cfg.Addr)
	if err != nil {
		return nil, err
	}
	if cfg.RootCAs != nil && !cfg.UseTLS {
		return nil, errors.New("qdrantstore: Config.RootCAs needs UseTLS")
	}
	var tlsConfig *tls.Config
	if cfg.UseTLS {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: cfg.RootCAs}
	}
	cli, err := qdrant.NewClient(&qdrant.Config{
		Host:      host,
		Port:      port,
		APIKey:    cfg.APIKey,
		UseTLS:    cfg.UseTLS,
		TLSConfig: tlsConfig,
		// Compatibility check adds a startup round-trip that
		// occasionally flakes on fresh qdrant pods; we pin the client
		// version in go.mod, the server version on the cluster side.
		SkipCompatibilityCheck: true,
		// With the compatibility check skipped, the client logs only its
		// warning about an API key sent without TLS, once per pooled
		// connection and for loopback hosts too. The server logs its own,
		// once (cmd/diderot newQdrantStore).
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: dial: %w", err)
	}
	if cfg.ShardNumber == 0 {
		cfg.ShardNumber = 1
	}
	if cfg.ReplicationFactor == 0 {
		cfg.ReplicationFactor = 1
	}
	return &Store{
		cli:               cli,
		addr:              cfg.Addr,
		tlsConfig:         tlsConfig,
		collection:        cfg.Collection,
		dimensions:        cfg.Dimensions,
		shardNumber:       cfg.ShardNumber,
		replicationFactor: cfg.ReplicationFactor,
		metrics:           newMetrics(reg, cfg.Collection),
	}, nil
}

// Collection returns the target collection name. Handy for logging.
func (s *Store) Collection() string { return s.collection }

// Dimensions returns the configured vector size.
func (s *Store) Dimensions() int { return int(s.dimensions) }

func (s *Store) requireDimensions(op string) error {
	if s.dimensions == 0 {
		return fmt.Errorf("qdrantstore: %s: Config.Dimensions is required", op)
	}
	return nil
}

// Close releases the underlying gRPC connection.
func (s *Store) Close() error { return s.cli.Close() }

// Ping makes one call to Qdrant, so a connection that cannot work fails
// before anything else uses the Store. Like every call, it fails at once,
// without retrying, when Qdrant cannot be reached.
func (s *Store) Ping(ctx context.Context) (err error) {
	timer := s.metrics.startTimer("ping")
	defer func() { timer.done(err) }()
	if _, err = s.cli.HealthCheck(ctx); err == nil {
		return nil
	}
	if status.Code(err) == codes.Unavailable {
		if tlsErr := s.explainTLS(ctx, err); tlsErr != nil {
			return tlsErr
		}
	}
	return fmt.Errorf("qdrantstore: health check: %w", err)
}

// CollectionExists reports whether the collection exists.
func (s *Store) CollectionExists(ctx context.Context) (bool, error) {
	exists, err := s.cli.CollectionExists(ctx, s.collection)
	if err != nil {
		return false, fmt.Errorf("qdrantstore: collection exists check: %w", err)
	}
	return exists, nil
}

// explainTLS tells the operator which TLS setting doesn't match Qdrant
// after a call failed with err, or returns nil when TLS isn't the problem.
// gRPC reports a failed handshake only as text, and may report a different
// address's failure when the host has several, so it shakes hands with
// Qdrant once more itself, reads the typed error, and for a certificate
// problem reports that error, which names what the certificate covers. Without TLS
// configured, any certificate, trusted or not, shows that Qdrant wants TLS.
func (s *Store) explainTLS(ctx context.Context, err error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	config := s.tlsConfig
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	conn, handshakeErr := (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", s.addr)
	if handshakeErr == nil {
		_ = conn.Close()
	}
	var (
		notTLS   tls.RecordHeaderError
		verify   *tls.CertificateVerificationError
		hostname x509.HostnameError
		invalid  x509.CertificateInvalidError
	)
	if s.tlsConfig == nil {
		if handshakeErr == nil || errors.As(handshakeErr, &verify) {
			return fmt.Errorf("DIDEROT_QDRANT_TLS is off, but Qdrant at %s requires TLS: %w", s.addr, err)
		}
		return nil
	}
	switch {
	case errors.As(handshakeErr, &notTLS):
		return fmt.Errorf("DIDEROT_QDRANT_TLS is on, but Qdrant at %s does not speak TLS: %w", s.addr, err)
	case errors.As(handshakeErr, &hostname):
		return fmt.Errorf("the certificate Qdrant at %s presented does not name that host; set DIDEROT_QDRANT_ADDR to a name it lists: %w", s.addr, handshakeErr)
	case errors.As(handshakeErr, &invalid):
		return fmt.Errorf("the certificate Qdrant at %s presented is not valid: %w", s.addr, handshakeErr)
	case errors.As(handshakeErr, &verify) && s.tlsConfig.RootCAs != nil:
		return fmt.Errorf("the certificate Qdrant at %s presented is not signed by a CA in DIDEROT_QDRANT_CA_FILE: %w", s.addr, handshakeErr)
	case errors.As(handshakeErr, &verify):
		return fmt.Errorf("the certificate Qdrant at %s presented is not signed by a CA this system trusts; set DIDEROT_QDRANT_CA_FILE to the CA that signed it: %w", s.addr, handshakeErr)
	}
	return nil
}

type payloadIndexSpec struct {
	field      string
	fieldType  qdrant.FieldType
	schemaType qdrant.PayloadSchemaType
}

var requiredPayloadIndexes = []payloadIndexSpec{
	{"source", qdrant.FieldType_FieldTypeKeyword, qdrant.PayloadSchemaType_Keyword},
	{"actor_id", qdrant.FieldType_FieldTypeKeyword, qdrant.PayloadSchemaType_Keyword},
	{"actor_canonical", qdrant.FieldType_FieldTypeKeyword, qdrant.PayloadSchemaType_Keyword},
	{"occurred_at", qdrant.FieldType_FieldTypeDatetime, qdrant.PayloadSchemaType_Datetime},
	{"scope", qdrant.FieldType_FieldTypeKeyword, qdrant.PayloadSchemaType_Keyword},
}

const (
	writeConsistencyFactor = uint32(1)
	hnswM                  = uint64(16)
	hnswEFConstruct        = uint64(100)
	maxOptimizationThreads = uint64(1)
)

// EnsureCollection creates the collection with Diderot's fixed storage and
// search shape if it does not exist. Existing collections are read and
// validated first; only genuinely missing payload indexes are created. This
// keeps ordinary application restarts read-only instead of broadcasting six
// redundant schema mutations to every shard.
//
// Shape:
//   - Config.Dimensions cosine
//   - scalar int8 quantization, always-in-ram (keeps recall; memory
//     footprint 4x smaller than fp32)
//   - float32 originals and payload on disk
//   - HNSW in RAM (m=16, ef_construct=100)
//   - one optimizer job per shard
//   - write consistency 1 so one unavailable replica does not stop writes
//
// Qdrant v1.16 still represents placement with the legacy on_disk/always_ram
// protobuf fields; go-client v1.18 deprecates them before exposing replacements.
//
//nolint:staticcheck
func (s *Store) EnsureCollection(ctx context.Context) (err error) {
	timer := s.metrics.startTimer("ensure_collection")
	defer func() { timer.done(err) }()
	if err = s.requireDimensions("ensure collection"); err != nil {
		return err
	}

	exists, err := s.cli.CollectionExists(ctx, s.collection)
	if err != nil {
		return fmt.Errorf("qdrantstore: collection exists check: %w", err)
	}
	if !exists {
		alwaysRAM := true
		onDisk := true
		hnswOnDisk := false
		err := s.cli.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: s.collection,
			VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
				Size:     s.dimensions,
				Distance: qdrant.Distance_Cosine,
				OnDisk:   &onDisk,
			}),
			QuantizationConfig: qdrant.NewQuantizationScalar(&qdrant.ScalarQuantization{
				Type:      qdrant.QuantizationType_Int8,
				AlwaysRam: &alwaysRAM,
			}),
			ShardNumber:            &s.shardNumber,
			ReplicationFactor:      &s.replicationFactor,
			WriteConsistencyFactor: qdrant.PtrOf(writeConsistencyFactor),
			OnDiskPayload:          &onDisk,
			HnswConfig: &qdrant.HnswConfigDiff{
				M:           qdrant.PtrOf(hnswM),
				EfConstruct: qdrant.PtrOf(hnswEFConstruct),
				OnDisk:      &hnswOnDisk,
			},
			OptimizersConfig: &qdrant.OptimizersConfigDiff{
				MaxOptimizationThreads: qdrant.NewMaxOptimizationThreads(maxOptimizationThreads),
			},
		})
		if err != nil {
			return fmt.Errorf("qdrantstore: create collection %q: %w", s.collection, err)
		}
	}

	info, err := s.cli.GetCollectionInfo(ctx, s.collection)
	if err != nil {
		return fmt.Errorf("qdrantstore: get collection %q: %w", s.collection, err)
	}
	if err := s.validateCollection(info); err != nil {
		return err
	}

	for _, idx := range requiredPayloadIndexes {
		if _, ok := info.GetPayloadSchema()[idx.field]; ok {
			continue
		}
		ft := idx.fieldType
		_, err := s.cli.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: s.collection,
			FieldName:      idx.field,
			FieldType:      &ft,
		})
		if err != nil {
			return fmt.Errorf("qdrantstore: create field index %q: %w", idx.field, err)
		}
	}
	return nil
}

//nolint:staticcheck // See EnsureCollection: Qdrant v1.16 is supported.
func (s *Store) validateCollection(info *qdrant.CollectionInfo) error {
	var mismatches []string
	config := info.GetConfig()
	params := config.GetParams()
	vectors := params.GetVectorsConfig().GetParams()
	if vectors == nil {
		mismatches = append(mismatches, "vectors must be a single unnamed vector")
	} else {
		if vectors.GetSize() != s.dimensions {
			mismatches = append(mismatches, fmt.Sprintf("vector size=%d want=%d", vectors.GetSize(), s.dimensions))
		}
		if vectors.GetDistance() != qdrant.Distance_Cosine {
			mismatches = append(mismatches, fmt.Sprintf("distance=%s want=Cosine", vectors.GetDistance()))
		}
		if !vectors.GetOnDisk() {
			mismatches = append(mismatches, "original vectors must be on disk")
		}
	}
	if params.GetShardNumber() != s.shardNumber {
		mismatches = append(mismatches, fmt.Sprintf("shards=%d want=%d", params.GetShardNumber(), s.shardNumber))
	}
	if params.GetReplicationFactor() != s.replicationFactor {
		mismatches = append(mismatches, fmt.Sprintf("replication=%d want=%d", params.GetReplicationFactor(), s.replicationFactor))
	}
	if params.GetWriteConsistencyFactor() != writeConsistencyFactor {
		mismatches = append(mismatches, fmt.Sprintf("write consistency=%d want=%d", params.GetWriteConsistencyFactor(), writeConsistencyFactor))
	}
	if !params.GetOnDiskPayload() {
		mismatches = append(mismatches, "payload must be on disk")
	}

	hnsw := config.GetHnswConfig()
	if hnsw.GetM() != hnswM || hnsw.GetEfConstruct() != hnswEFConstruct || hnsw.GetOnDisk() {
		mismatches = append(mismatches, fmt.Sprintf("HNSW=(m=%d ef_construct=%d on_disk=%t) want=(m=%d ef_construct=%d on_disk=false)",
			hnsw.GetM(), hnsw.GetEfConstruct(), hnsw.GetOnDisk(), hnswM, hnswEFConstruct))
	}
	quantization := config.GetQuantizationConfig().GetScalar()
	if quantization == nil || quantization.GetType() != qdrant.QuantizationType_Int8 || !quantization.GetAlwaysRam() {
		mismatches = append(mismatches, "quantization must be scalar int8 with always_ram=true")
	}
	optimizerThreads := config.GetOptimizerConfig().GetMaxOptimizationThreads()
	if optimizerThreads == nil || optimizerThreads.GetValue() != maxOptimizationThreads {
		mismatches = append(mismatches, fmt.Sprintf("max optimization threads=%d want=%d", optimizerThreads.GetValue(), maxOptimizationThreads))
	}

	for _, idx := range requiredPayloadIndexes {
		if actual, ok := info.GetPayloadSchema()[idx.field]; ok && actual.GetDataType() != idx.schemaType {
			mismatches = append(mismatches, fmt.Sprintf("payload index %q=%s want=%s", idx.field, actual.GetDataType(), idx.schemaType))
		}
	}
	if len(mismatches) > 0 {
		return &ShapeError{Collection: s.collection, Mismatches: mismatches}
	}
	return nil
}

// ShapeError is a collection that exists with another shape than this
// deployment needs: a different vector size, sharding, or index settings.
type ShapeError struct {
	Collection string
	Mismatches []string
}

func (e *ShapeError) Error() string {
	return fmt.Sprintf("qdrantstore: collection %q configuration mismatch: %s", e.Collection, strings.Join(e.Mismatches, "; "))
}

// Point is the Store-level representation of one chunk's vector +
// payload. ID is event_chunks.id. Payload matches the design doc
// shape: {event_id, source, actor_id, occurred_at, thread_id}.
// Callers populate every field; omitting any means filters on that
// field reject the point.
type Point struct {
	ID      uint64
	Vector  []float32
	EventID int64
	Source  string
	ActorID string // "" if the source has no actor (rare)
	// ActorCanonical is the cross-source identity key (email or
	// 'slack:<user_id>'); empty if the canonical wasn't resolved at
	// ingest time. Lets the model retrieve "events authored by this
	// asker" with one key regardless of source. Filter logic ORs
	// matches against actor_id and actor_canonical so callers can
	// pass either form.
	ActorCanonical string
	ThreadID       string    // "" if the event is not in a thread
	OccurredAt     time.Time // stored as RFC3339 string in the payload
	// Scope is the event's visibility scope (events.scope column,
	// denormalized so the vector leg can pre-filter). A point with an
	// empty scope never matches a scoped search (deny-by-default);
	// the embedder always populates it from the events row.
	Scope string
}

// Upsert writes a batch of points. Idempotent: re-upserting the same
// id overwrites the prior vector + payload. The embedder relies on
// this — if it crashes between a successful qdrant upsert and marking
// event_chunks.embedded_at, the next run re-fetches the chunk and the
// upsert is a no-op.
func (s *Store) Upsert(ctx context.Context, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	timer := s.metrics.startTimer("upsert")
	var err error
	defer func() { timer.done(err) }()
	if err = s.requireDimensions("upsert"); err != nil {
		return err
	}

	structs := make([]*qdrant.PointStruct, len(points))
	for i, p := range points {
		if uint64(len(p.Vector)) != s.dimensions {
			err = fmt.Errorf("qdrantstore: point %d: vector len %d, want %d",
				p.ID, len(p.Vector), s.dimensions)
			return err
		}
		structs[i] = &qdrant.PointStruct{
			Id:      qdrant.NewIDNum(p.ID),
			Vectors: qdrant.NewVectors(p.Vector...),
			Payload: qdrant.NewValueMap(map[string]any{
				"event_id":        p.EventID,
				"source":          p.Source,
				"actor_id":        p.ActorID,
				"actor_canonical": p.ActorCanonical,
				"thread_id":       p.ThreadID,
				"occurred_at":     p.OccurredAt.UTC().Format(time.RFC3339Nano),
				"scope":           p.Scope,
			}),
		}
	}
	wait := true
	_, err = s.cli.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: s.collection,
		Points:         structs,
		Wait:           &wait, // synchronous semantics: Upsert returns after the write is visible
	})
	if err != nil {
		return fmt.Errorf("qdrantstore: upsert %d points: %w", len(points), err)
	}
	return nil
}

// DeleteIDs removes chunk ids from the collection. The durable Postgres
// delete worker is its production caller. Deleting a missing id is a no-op.
func (s *Store) DeleteIDs(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	timer := s.metrics.startTimer("delete")
	var err error
	defer func() { timer.done(err) }()

	pointIDs := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pointIDs[i] = qdrant.NewIDNum(id)
	}
	wait := true
	_, err = s.cli.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: s.collection,
		Points:         qdrant.NewPointsSelector(pointIDs...),
		Wait:           &wait,
	})
	if err != nil {
		return fmt.Errorf("qdrantstore: delete %d points: %w", len(ids), err)
	}
	return nil
}

// Drop deletes the store's collection entirely. Test and explicit local-dev
// infrastructure use it with dedicated collections; each integration test creates a uniquely-named collection, and
// without a paired delete they accumulate in the dev qdrant volume
// until the container OOMs (each collection holds open segment
// files + an HNSW arena even when idle). Production code never
// calls this.
func (s *Store) Drop(ctx context.Context) error {
	if err := s.cli.DeleteCollection(ctx, s.collection); err != nil {
		return fmt.Errorf("qdrantstore: drop collection %q: %w", s.collection, err)
	}
	return nil
}

// SetScope patches the scope payload field without touching the vector or
// other payload fields. Missing points fail the whole call. The durable scope
// worker retains that batch for a later bounded retry.
func (s *Store) SetScope(ctx context.Context, ids []uint64, scope string) error {
	if len(ids) == 0 {
		return nil
	}
	timer := s.metrics.startTimer("set_scope")
	var err error
	defer func() { timer.done(err) }()

	pointIDs := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pointIDs[i] = qdrant.NewIDNum(id)
	}
	wait := true
	_, err = s.cli.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: s.collection,
		Payload:        qdrant.NewValueMap(map[string]any{"scope": scope}),
		PointsSelector: qdrant.NewPointsSelector(pointIDs...),
		Wait:           &wait,
	})
	if err != nil {
		return fmt.Errorf("qdrantstore: set scope on %d points: %w", len(ids), err)
	}
	return nil
}

func (s *Store) SetOccurredAt(ctx context.Context, ids []uint64, occurredAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	timer := s.metrics.startTimer("set_occurred_at")
	var err error
	defer func() { timer.done(err) }()
	pointIDs := make([]*qdrant.PointId, len(ids))
	for index, id := range ids {
		pointIDs[index] = qdrant.NewIDNum(id)
	}
	wait := true
	_, err = s.cli.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: s.collection,
		Payload:        qdrant.NewValueMap(map[string]any{"occurred_at": occurredAt.UTC().Format(time.RFC3339Nano)}),
		PointsSelector: qdrant.NewPointsSelector(pointIDs...),
		Wait:           &wait,
	})
	if err != nil {
		return fmt.Errorf("qdrantstore: set recency on %d points: %w", len(ids), err)
	}
	return nil
}

// SetSource patches the `source` payload field on the given chunk ids without
// touching vectors or other payload fields (SetPayload merges). When a
// source is renamed, `source` is the only payload field whose authoritative
// value changes, so the refresh is a payload patch — never a re-embed of
// unchanged content.
//
// Shares SetScope's NOT-silently-idempotent-on-missing-ids caveat: SetPayload
// against an id that no longer exists fails the whole batch as gRPC NotFound.
// Callers retry per-point on IsNotFound, exactly as scopesync does.
func (s *Store) SetSource(ctx context.Context, ids []uint64, source string) error {
	if len(ids) == 0 {
		return nil
	}
	timer := s.metrics.startTimer("set_source")
	var err error
	defer func() { timer.done(err) }()

	pointIDs := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pointIDs[i] = qdrant.NewIDNum(id)
	}
	wait := true
	_, err = s.cli.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: s.collection,
		Payload:        qdrant.NewValueMap(map[string]any{"source": source}),
		PointsSelector: qdrant.NewPointsSelector(pointIDs...),
		Wait:           &wait,
	})
	if err != nil {
		return fmt.Errorf("qdrantstore: set source on %d points: %w", len(ids), err)
	}
	return nil
}

// IsNotFound reports whether err is (or wraps) a qdrant gRPC NotFound
// status — e.g. SetPayload against a point id that no longer exists.
// The durable scope worker uses this to identify a repeated
// Postgres/Qdrant inconsistency. Other cutover paths may use it for policy.
func IsNotFound(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.NotFound
}

// IsRateLimited reports whether err is (or wraps) a qdrant gRPC
// ResourceExhausted status — qdrant returns this when a write batch
// hits its internal rate limiter (e.g. "too_many_internal_resets").
func IsRateLimited(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.ResourceExhausted
}

// SearchFilters is the structured filter shape /v1/ask passes to
// Search. Zero values mean "no constraint on this dimension". All
// active filters AND together.
type SearchFilters struct {
	// Sources restricts to points whose payload `source` is any of
	// these values. Empty = all sources.
	Sources []string
	// Actors restricts to points whose payload `actor_id` is any of
	// these values. Empty = all actors.
	Actors []string
	// OccurredAfter / OccurredBefore constrain the occurred_at payload.
	// Zero-value time.Time means unbounded on that side.
	OccurredAfter  time.Time
	OccurredBefore time.Time
	// AllowedScopes restricts to points whose payload `scope` is any
	// of these values. Empty/nil grants no access.
	// Non-empty = hard visibility boundary: a point whose scope is
	// not in the set is excluded pre-ANN (filterable HNSW), so
	// restricted content never reaches ranking, snippets, or the
	// model. Set server-side by the ask layer; never
	// model-controlled.
	AllowedScopes []string
	// ExcludePointIDs leaves these points (chunk ids) out before ranking,
	// so a search still returns as many results as were asked for.
	ExcludePointIDs []uint64
}

// Hit is one qdrant search result: the chunk id (= qdrant point id)
// and its score. Callers dereference the chunk content from Postgres
// using the id — the vector + payload on its own doesn't give us the
// text that matched.
type Hit struct {
	ChunkID uint64
	EventID int64
	Score   float32
	// Source is set by SearchEachSource only: the source the hit was
	// ranked within.
	Source string
}

// maxSearchedSources bounds the groups of one SearchEachSource request. It
// is far above the number of sources a build registers.
const maxSearchedSources = 64

// SearchEachSource ranks every source on its own, in one request: points are
// grouped by their source and the best perSource of each group come back.
// One query over everything returns whatever is nearest, and a source with
// millions of points fills that list long before a source with a thousand
// gets a place in it, however good its match. Hits are best first within a
// source and in no order across sources.
func (s *Store) SearchEachSource(
	ctx context.Context,
	vector []float32,
	filters SearchFilters,
	perSource uint64,
) ([]Hit, error) {
	timer := s.metrics.startTimer("search_each_source")
	var err error
	defer func() { timer.done(err) }()

	if err = s.requireDimensions("search"); err != nil {
		return nil, err
	}
	if uint64(len(vector)) != s.dimensions {
		err = fmt.Errorf("qdrantstore: search: vector len %d, want %d", len(vector), s.dimensions)
		return nil, err
	}
	if len(events.ReadableScopes(filters.AllowedScopes)) == 0 {
		return nil, nil
	}
	groups := uint64(maxSearchedSources)
	req := &qdrant.QueryPointGroups{
		CollectionName: s.collection,
		Query:          qdrant.NewQuery(vector...),
		GroupBy:        "source",
		GroupSize:      &perSource,
		Limit:          &groups,
		WithPayload:    qdrant.NewWithPayload(true),
	}
	if f := buildFilter(filters); f != nil {
		req.Filter = f
	}
	result, err := s.cli.QueryGroups(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: query groups: %w", err)
	}
	var hits []Hit
	for _, group := range result {
		source := group.GetId().GetStringValue()
		for _, p := range group.GetHits() {
			h := Hit{Score: p.Score, Source: source}
			if id := p.GetId(); id != nil {
				h.ChunkID = id.GetNum()
			}
			if payload := p.GetPayload(); payload != nil {
				if v, ok := payload["event_id"]; ok {
					h.EventID = v.GetIntegerValue()
				}
			}
			hits = append(hits, h)
		}
	}
	return hits, nil
}

// Search runs a single vector query. Callers over-fetch (topK=100 is
// the /v1/ask default) and dedup by event_id downstream.
func (s *Store) Search(
	ctx context.Context,
	vector []float32,
	filters SearchFilters,
	topK uint64,
) ([]Hit, error) {
	timer := s.metrics.startTimer("search")
	var err error
	defer func() { timer.done(err) }()

	if err = s.requireDimensions("search"); err != nil {
		return nil, err
	}
	if uint64(len(vector)) != s.dimensions {
		err = fmt.Errorf("qdrantstore: search: vector len %d, want %d", len(vector), s.dimensions)
		return nil, err
	}
	if len(events.ReadableScopes(filters.AllowedScopes)) == 0 {
		return nil, nil
	}
	req := &qdrant.QueryPoints{
		CollectionName: s.collection,
		Query:          qdrant.NewQuery(vector...),
		Limit:          &topK,
		WithPayload:    qdrant.NewWithPayload(true),
	}
	if f := buildFilter(filters); f != nil {
		req.Filter = f
	}
	points, err := s.cli.Query(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: query: %w", err)
	}
	hits := make([]Hit, 0, len(points))
	for _, p := range points {
		h := Hit{Score: p.Score}
		if id := p.GetId(); id != nil {
			h.ChunkID = id.GetNum()
		}
		if payload := p.GetPayload(); payload != nil {
			if v, ok := payload["event_id"]; ok {
				h.EventID = v.GetIntegerValue()
			}
		}
		hits = append(hits, h)
	}
	return hits, nil
}

// CountPoints returns the current point count for our collection.
func (s *Store) CountPoints(ctx context.Context) (uint64, error) {
	timer := s.metrics.startTimer("count")
	var err error
	defer func() { timer.done(err) }()

	count, err := s.cli.Count(ctx, &qdrant.CountPoints{
		CollectionName: s.collection,
	})
	if err != nil {
		return 0, fmt.Errorf("qdrantstore: count: %w", err)
	}
	return count, nil
}

// ScrollIDs yields every point id in the collection in batches of pageSize.
// It is retained for diagnostics and tests. Normal repair work never scrolls
// the collection.
//
// Returning an error from visit aborts the scroll. Returning io.EOF
// is not special — visit is called only with non-empty batches.
func (s *Store) ScrollIDs(
	ctx context.Context,
	pageSize uint32,
	visit func([]uint64) error,
) error {
	timer := s.metrics.startTimer("scroll")
	var err error
	defer func() { timer.done(err) }()

	if pageSize == 0 {
		pageSize = 1000
	}
	var offset *qdrant.PointId
	withPayload := false
	withVectors := false
	for {
		req := &qdrant.ScrollPoints{
			CollectionName: s.collection,
			Limit:          &pageSize,
			Offset:         offset,
			WithPayload:    qdrant.NewWithPayload(withPayload),
			WithVectors:    qdrant.NewWithVectors(withVectors),
		}
		var pts []*qdrant.RetrievedPoint
		pts, offset, err = s.cli.ScrollAndOffset(ctx, req)
		if err != nil {
			return fmt.Errorf("qdrantstore: scroll: %w", err)
		}
		if len(pts) > 0 {
			ids := make([]uint64, 0, len(pts))
			for _, p := range pts {
				if id := p.GetId(); id != nil {
					ids = append(ids, id.GetNum())
				}
			}
			if len(ids) > 0 {
				if err = visit(ids); err != nil {
					return err
				}
			}
		}
		if offset == nil {
			return nil
		}
	}
}

// ScrollSource yields matching points' id, `source` payload string, and
// `event_id` payload integer in batches of `pageSize`, calling `visit` once
// per point. The source-cutover uses this to enumerate points still carrying
// a stale source alias and resolve each to its authoritative Postgres event.
//
// Points whose payload lacks `source` or `event_id` are yielded with an empty
// string / zero value — callers must inspect the fields and decide, never
// assume an event id (or, worse, a session id) is present. event_id is the
// integer EventID written at Upsert time; it is NOT a session id.
//
// Returning an error from visit aborts the scroll.
func (s *Store) ScrollSource(
	ctx context.Context,
	sourceFilter string,
	pageSize uint32,
	visit func(id uint64, source string, eventID int64) error,
) error {
	timer := s.metrics.startTimer("scroll_source")
	var err error
	defer func() { timer.done(err) }()

	if pageSize == 0 {
		pageSize = 1000
	}
	var offset *qdrant.PointId
	withPayload := true
	withVectors := false
	for {
		req := &qdrant.ScrollPoints{
			CollectionName: s.collection,
			Limit:          &pageSize,
			Offset:         offset,
			WithPayload:    qdrant.NewWithPayload(withPayload),
			WithVectors:    qdrant.NewWithVectors(withVectors),
		}
		if sourceFilter != "" {
			req.Filter = &qdrant.Filter{Must: []*qdrant.Condition{qdrant.NewMatchKeyword("source", sourceFilter)}}
		}
		var pts []*qdrant.RetrievedPoint
		pts, offset, err = s.cli.ScrollAndOffset(ctx, req)
		if err != nil {
			return fmt.Errorf("qdrantstore: scroll source: %w", err)
		}
		for _, p := range pts {
			id := uint64(0)
			if pid := p.GetId(); pid != nil {
				id = pid.GetNum()
			}
			var source string
			var eventID int64
			if payload := p.GetPayload(); payload != nil {
				if v, ok := payload["source"]; ok {
					source = v.GetStringValue()
				}
				if v, ok := payload["event_id"]; ok {
					eventID = v.GetIntegerValue()
				}
			}
			if err = visit(id, source, eventID); err != nil {
				return err
			}
		}
		if offset == nil {
			return nil
		}
	}
}

// ScrollScopes yields every point id and its scope payload for the explicit
// cutover audit. Vectors and all unrelated payload fields stay server-side.
func (s *Store) ScrollScopes(
	ctx context.Context,
	pageSize uint32,
	visit func(ids []uint64, scopes []string) error,
) error {
	timer := s.metrics.startTimer("scroll_scopes")
	var err error
	defer func() { timer.done(err) }()

	if pageSize == 0 {
		pageSize = 1000
	}
	var offset *qdrant.PointId
	withVectors := false
	for {
		req := &qdrant.ScrollPoints{
			CollectionName: s.collection,
			Limit:          &pageSize,
			Offset:         offset,
			WithPayload:    qdrant.NewWithPayloadInclude("scope"),
			WithVectors:    qdrant.NewWithVectors(withVectors),
		}
		var points []*qdrant.RetrievedPoint
		points, offset, err = s.cli.ScrollAndOffset(ctx, req)
		if err != nil {
			return fmt.Errorf("qdrantstore: scroll scopes: %w", err)
		}
		if len(points) > 0 {
			ids := make([]uint64, 0, len(points))
			scopes := make([]string, 0, len(points))
			for _, point := range points {
				if id := point.GetId(); id != nil {
					ids = append(ids, id.GetNum())
					scope := ""
					if value, ok := point.GetPayload()["scope"]; ok {
						scope = value.GetStringValue()
					}
					scopes = append(scopes, scope)
				}
			}
			if len(ids) > 0 {
				if err = visit(ids, scopes); err != nil {
					return err
				}
			}
		}
		if offset == nil {
			return nil
		}
	}
}

// buildFilter turns SearchFilters into a scope-restricted *qdrant.Filter.
//
// The Actors filter ORs across two payload fields:
// `actor_id` (legacy: slack user_id, agent_session email) and
// `actor_canonical` (post-AC-Step-6: email or 'slack:<user_id>').
// This means callers can pass any of (display name, slack user_id,
// email) and match the right rows regardless of which form was
// indexed at upsert time. Pre-AC-Step-4 points only have actor_id
// populated; new points carry both.
func buildFilter(f SearchFilters) *qdrant.Filter {
	var must []*qdrant.Condition
	if len(f.Sources) > 0 {
		must = append(must, qdrant.NewMatchKeywords("source", f.Sources...))
	}
	if len(f.Actors) > 0 {
		// Inner Filter with Should = OR: a point matches if ANY of
		// its actor_id / actor_canonical payload fields is in the
		// supplied set. Wrap the inner filter as a single Must
		// condition so it AND-composes with source / time clauses.
		actorFilter := &qdrant.Filter{
			Should: []*qdrant.Condition{
				qdrant.NewMatchKeywords("actor_id", f.Actors...),
				qdrant.NewMatchKeywords("actor_canonical", f.Actors...),
			},
		}
		must = append(must, qdrant.NewFilterAsCondition(actorFilter))
	}
	if !f.OccurredAfter.IsZero() || !f.OccurredBefore.IsZero() {
		r := &qdrant.DatetimeRange{}
		if !f.OccurredAfter.IsZero() {
			r.Gte = timeToProto(f.OccurredAfter)
		}
		if !f.OccurredBefore.IsZero() {
			r.Lte = timeToProto(f.OccurredBefore)
		}
		must = append(must, qdrant.NewDatetimeRange("occurred_at", r))
	}
	must = append(must, qdrant.NewMatchKeywords("scope", events.ReadableScopes(f.AllowedScopes)...))
	filter := &qdrant.Filter{Must: must}
	if len(f.ExcludePointIDs) > 0 {
		ids := make([]*qdrant.PointId, len(f.ExcludePointIDs))
		for i, id := range f.ExcludePointIDs {
			ids[i] = qdrant.NewIDNum(id)
		}
		filter.MustNot = []*qdrant.Condition{qdrant.NewHasID(ids...)}
	}
	return filter
}

func timeToProto(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t)
}
