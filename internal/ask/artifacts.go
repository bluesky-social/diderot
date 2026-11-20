package ask

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	PanelImageToolName        = "grafana_get_panel_image"
	maxPanelImageBytes        = 10 << 20
	maxPanelImageDimension    = 4096
	maxPanelImagePixels       = 16_000_000
	maxSelectedArtifacts      = 4
	maxArtifactTitleRunes     = 160
	maxArtifactAltTextRunes   = 600
	panelArtifactSchema       = "ask-panel-artifact-v1"
	panelArtifactStorageScope = "ask-artifacts"
)

type ArtifactBlobStore interface {
	Put(ctx context.Context, key string, contentType string, data []byte) error
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)
	Bucket() string
}

type ArtifactDescriptor struct {
	ID        uuid.UUID `json:"id"`
	MediaType string    `json:"media_type"`
	ByteSize  int64     `json:"byte_size"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Title     string    `json:"title"`
	AltText   string    `json:"alt_text"`
	URL       string    `json:"url"`
}

type ArtifactSelection struct {
	ArtifactID uuid.UUID `json:"artifact_id"`
	Title      string    `json:"title"`
	AltText    string    `json:"alt_text"`
}

type ArtifactRecord struct {
	ID          uuid.UUID
	SessionID   uuid.UUID
	ToolCallID  string
	ContentHash string
	StorageKey  string
	MediaType   string
	ByteSize    int64
	Width       int
	Height      int
	SourceTool  string
	Status      string
	CreatedAt   time.Time
}

type PanelArtifactMetadata struct {
	Schema     string    `json:"schema"`
	ArtifactID uuid.UUID `json:"artifact_id"`
	MediaType  string    `json:"media_type"`
	ByteSize   int64     `json:"byte_size"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	DeepLink   string    `json:"grafana_url,omitempty"`
}

type ArtifactStore struct {
	pool *pgxpool.Pool
	blob ArtifactBlobStore
}

type artifactSessionContextKey struct{}

func withArtifactSession(ctx context.Context, sessionID uuid.UUID) context.Context {
	return context.WithValue(ctx, artifactSessionContextKey{}, sessionID)
}

func artifactSessionFromContext(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(artifactSessionContextKey{}).(uuid.UUID)
	return id
}

func NewArtifactStore(pool *pgxpool.Pool, blob ArtifactBlobStore) *ArtifactStore {
	if pool == nil || blob == nil {
		return nil
	}
	return &ArtifactStore{pool: pool, blob: blob}
}

func (s *ArtifactStore) PersistPanelImage(ctx context.Context, sessionID uuid.UUID, callID string, data []byte) (ArtifactRecord, error) {
	if s == nil || s.pool == nil || s.blob == nil {
		return ArtifactRecord{}, errors.New("ask artifact store is unavailable")
	}
	if sessionID == uuid.Nil || strings.TrimSpace(callID) == "" {
		return ArtifactRecord{}, errors.New("ask artifact session and tool call are required")
	}
	width, height, err := validatePanelPNG(data)
	if err != nil {
		return ArtifactRecord{}, err
	}
	hashBytes := sha256.Sum256(data)
	contentHash := hex.EncodeToString(hashBytes[:])
	storageKey := fmt.Sprintf("%s/%s/%s.png", panelArtifactStorageScope, contentHash[:2], contentHash)
	if err := s.blob.Put(ctx, storageKey, "image/png", data); err != nil {
		return ArtifactRecord{}, fmt.Errorf("store panel image: %w", err)
	}
	verified, contentType, err := s.blob.Get(ctx, storageKey)
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("verify panel image: %w", err)
	}
	if contentType != "" && contentType != "image/png" {
		return ArtifactRecord{}, fmt.Errorf("verify panel image: content type %q", contentType)
	}
	if !bytes.Equal(verified, data) {
		return ArtifactRecord{}, errors.New("verify panel image: stored bytes differ")
	}

	idempotency := hashText(sessionID.String() + "\n" + callID + "\n" + contentHash)
	record := ArtifactRecord{ID: uuid.New(), SessionID: sessionID, ToolCallID: callID, ContentHash: contentHash, StorageKey: storageKey, MediaType: "image/png", ByteSize: int64(len(data)), Width: width, Height: height, SourceTool: PanelImageToolName, Status: "ready"}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO ask_artifacts
		       (id, session_id, tool_call_id, content_sha256, storage_key,
		        media_type, byte_size, width, height, source_tool, status,
		        idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'ready',$11)
		ON CONFLICT (idempotency_key) DO UPDATE
		SET idempotency_key = EXCLUDED.idempotency_key
		RETURNING id, created_at`,
		record.ID, record.SessionID, record.ToolCallID, record.ContentHash,
		record.StorageKey, record.MediaType, record.ByteSize, record.Width,
		record.Height, record.SourceTool, idempotency,
	).Scan(&record.ID, &record.CreatedAt)
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("catalog panel image: %w", err)
	}
	return record, nil
}

func validatePanelPNG(data []byte) (int, int, error) {
	if len(data) == 0 || len(data) > maxPanelImageBytes {
		return 0, 0, fmt.Errorf("panel image is %d bytes; allowed range is 1..%d", len(data), maxPanelImageBytes)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("panel image is not a valid PNG: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxPanelImageDimension || cfg.Height > maxPanelImageDimension || int64(cfg.Width)*int64(cfg.Height) > maxPanelImagePixels {
		return 0, 0, fmt.Errorf("panel image dimensions %dx%d exceed the accepted bounds", cfg.Width, cfg.Height)
	}
	return cfg.Width, cfg.Height, nil
}

func (s *ArtifactStore) ValidateSelections(ctx context.Context, sessionID uuid.UUID, selections []ArtifactSelection) error {
	if err := validateArtifactSelectionShape(selections); err != nil {
		return err
	}
	for _, selection := range selections {
		var exists bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM ask_artifacts
			   WHERE id=$1 AND session_id=$2 AND status='ready' AND source_tool=$3
			)`, selection.ArtifactID, sessionID, PanelImageToolName).Scan(&exists); err != nil {
			return fmt.Errorf("validate artifact %s: %w", selection.ArtifactID, err)
		}
		if !exists {
			return fmt.Errorf("artifact %s is not a ready panel image in this session", selection.ArtifactID)
		}
	}
	return nil
}

func (s *ArtifactStore) RecordSteering(ctx context.Context, sessionID uuid.UUID, userTurn int, steering ObservabilitySteering) error {
	if s == nil {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ask_observability_steering
		       (session_id,user_turn,intent,router_version,router_hash,bundle_version,bundle_hash,card_ids)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (session_id,user_turn) DO NOTHING`,
		sessionID, userTurn, steering.Intent, ObservabilityRouterVersion,
		steering.RouterHash, ObservabilityBundleVersion, steering.BundleHash,
		sortedCardIDs(steering.CardIDs))
	return err
}

func (s *ArtifactStore) LoadResultArtifacts(ctx context.Context, sessionID uuid.UUID, assistantTurn int) ([]ArtifactDescriptor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.media_type, a.byte_size, a.width, a.height,
		       r.title, r.alt_text
		  FROM ask_result_artifacts r
		  JOIN ask_artifacts a ON a.id=r.artifact_id
		 WHERE r.session_id=$1 AND r.assistant_turn=$2 AND a.status='ready'
		 ORDER BY r.position`, sessionID, assistantTurn)
	if err != nil {
		return nil, fmt.Errorf("load result artifacts: %w", err)
	}
	defer rows.Close()
	var out []ArtifactDescriptor
	for rows.Next() {
		var d ArtifactDescriptor
		if err := rows.Scan(&d.ID, &d.MediaType, &d.ByteSize, &d.Width, &d.Height, &d.Title, &d.AltText); err != nil {
			return nil, err
		}
		d.URL = fmt.Sprintf("/v1/ask/%s/artifacts/%s", sessionID, d.ID)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *ArtifactStore) loadRecord(ctx context.Context, sessionID, artifactID uuid.UUID) (ArtifactRecord, error) {
	var r ArtifactRecord
	err := s.pool.QueryRow(ctx, `
		SELECT id,session_id,tool_call_id,content_sha256,storage_key,media_type,
		       byte_size,width,height,source_tool,status,created_at
		  FROM ask_artifacts
		 WHERE id=$1 AND session_id=$2 AND status='ready'`, artifactID, sessionID).
		Scan(&r.ID, &r.SessionID, &r.ToolCallID, &r.ContentHash, &r.StorageKey,
			&r.MediaType, &r.ByteSize, &r.Width, &r.Height, &r.SourceTool, &r.Status, &r.CreatedAt)
	if err != nil {
		return ArtifactRecord{}, err
	}
	return r, nil
}

func (s *ArtifactStore) ReadArtifact(ctx context.Context, sessionID, artifactID uuid.UUID) (ArtifactRecord, []byte, error) {
	r, err := s.loadRecord(ctx, sessionID, artifactID)
	if err != nil {
		return ArtifactRecord{}, nil, err
	}
	data, contentType, err := s.blob.Get(ctx, r.StorageKey)
	if err != nil {
		s.markUnavailable(ctx, r.ID)
		return ArtifactRecord{}, nil, err
	}
	width, height, err := validatePanelPNG(data)
	if err != nil || (contentType != "" && contentType != r.MediaType) || int64(len(data)) != r.ByteSize || width != r.Width || height != r.Height || hashBytes(data) != r.ContentHash {
		s.markUnavailable(ctx, r.ID)
		return ArtifactRecord{}, nil, errors.New("stored Ask artifact failed integrity validation")
	}
	return r, data, nil
}

func (s *ArtifactStore) markUnavailable(ctx context.Context, id uuid.UUID) {
	_, _ = s.pool.Exec(ctx, `UPDATE ask_artifacts SET status='unavailable' WHERE id=$1 AND status='ready'`, id)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *ArtifactStore) HydrateHistory(ctx context.Context, sessionID uuid.UUID, history []StoredMessage) []StoredMessage {
	out := append([]StoredMessage(nil), history...)
	for i := range out {
		msg := out[i].Message
		if msg == nil || msg.Role != schema.AgenticRoleTypeUser {
			continue
		}
		for blockIndex, block := range msg.ContentBlocks {
			if block == nil || block.FunctionToolResult == nil || block.FunctionToolResult.Name != PanelImageToolName {
				continue
			}
			meta, ok := panelMetadataFromResult(block.FunctionToolResult)
			if !ok {
				continue
			}
			_, data, err := s.ReadArtifact(ctx, sessionID, meta.ArtifactID)
			if err != nil {
				continue
			}
			encoded := base64.StdEncoding.EncodeToString(data)
			resultCopy := *block.FunctionToolResult
			resultCopy.Content = append([]*schema.FunctionToolResultContentBlock(nil), block.FunctionToolResult.Content...)
			resultCopy.Content = append(resultCopy.Content, &schema.FunctionToolResultContentBlock{
				Type:  schema.FunctionToolResultContentBlockTypeImage,
				Image: &schema.UserInputImage{Base64Data: encoded, MIMEType: "image/png"},
			})
			blockCopy := *block
			blockCopy.FunctionToolResult = &resultCopy
			msgCopy := *msg
			msgCopy.ContentBlocks = append([]*schema.ContentBlock(nil), msg.ContentBlocks...)
			msgCopy.ContentBlocks[blockIndex] = &blockCopy
			out[i].Message = &msgCopy
		}
	}
	return out
}

func panelMetadataFromResult(result *schema.FunctionToolResult) (PanelArtifactMetadata, bool) {
	if result == nil {
		return PanelArtifactMetadata{}, false
	}
	for _, content := range result.Content {
		if content == nil || content.Type != schema.FunctionToolResultContentBlockTypeText || content.Text == nil {
			continue
		}
		var meta PanelArtifactMetadata
		if json.Unmarshal([]byte(content.Text.Text), &meta) == nil && meta.Schema == panelArtifactSchema && meta.ArtifactID != uuid.Nil {
			return meta, true
		}
	}
	return PanelArtifactMetadata{}, false
}

func (s *ArtifactStore) DeliveryExists(ctx context.Context, sessionID uuid.UUID, assistantTurn int, artifactID uuid.UUID, surface, target string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM ask_artifact_deliveries
		 WHERE session_id=$1 AND assistant_turn=$2 AND artifact_id=$3
		   AND surface=$4 AND target=$5 AND status='delivered')`,
		sessionID, assistantTurn, artifactID, surface, target).Scan(&exists)
	return exists, err
}

func (s *ArtifactStore) RecordDelivery(ctx context.Context, sessionID uuid.UUID, assistantTurn int, artifactID uuid.UUID, surface, target, externalID, status, failure string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ask_artifact_deliveries
		       (session_id,assistant_turn,artifact_id,surface,target,external_id,status,failure_reason)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,NULLIF($8,''))
		ON CONFLICT (session_id,assistant_turn,artifact_id,surface,target)
		DO UPDATE SET external_id=EXCLUDED.external_id,status=EXCLUDED.status,
		              failure_reason=EXCLUDED.failure_reason,updated_at=now()`,
		sessionID, assistantTurn, artifactID, surface, target, externalID, status, failure)
	return err
}

func (s *ArtifactStore) ArtifactRecord(ctx context.Context, sessionID, artifactID uuid.UUID) (ArtifactRecord, error) {
	return s.loadRecord(ctx, sessionID, artifactID)
}

func IsArtifactNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// validateArtifactSelectionShape validates selections and, in place, strips
// any embedded NUL byte (U+0000) from Title and AltText before checking
// their length: ask_result_artifacts.title and .alt_text are plain Postgres
// text columns that reject a literal NUL byte outright (SQLSTATE 22021,
// "invalid byte sequence"). These strings come from the same decoded
// structured final answer as the assistant's text (respond.go's
// decodeFinalResponse), so they can carry a NUL for the same reason the
// message persistence path can; stripping here, before every caller
// (ValidateSelections, decodeFinalResponse, and the terminal-commit insert
// itself) uses the selections, is the one place that needs to know about it.
func validateArtifactSelectionShape(selections []ArtifactSelection) error {
	if len(selections) > maxSelectedArtifacts {
		return fmt.Errorf("select at most %d artifacts", maxSelectedArtifacts)
	}
	seen := make(map[uuid.UUID]struct{}, len(selections))
	for i := range selections {
		selections[i].Title = stripNUL(selections[i].Title)
		selections[i].AltText = stripNUL(selections[i].AltText)
		selection := selections[i]
		if selection.ArtifactID == uuid.Nil {
			return fmt.Errorf("artifacts[%d].artifact_id is required", i)
		}
		if _, exists := seen[selection.ArtifactID]; exists {
			return fmt.Errorf("artifact %s is selected more than once", selection.ArtifactID)
		}
		seen[selection.ArtifactID] = struct{}{}
		if strings.TrimSpace(selection.Title) == "" || len([]rune(selection.Title)) > maxArtifactTitleRunes {
			return fmt.Errorf("artifacts[%d].title must contain 1..%d characters", i, maxArtifactTitleRunes)
		}
		if strings.TrimSpace(selection.AltText) == "" || len([]rune(selection.AltText)) > maxArtifactAltTextRunes {
			return fmt.Errorf("artifacts[%d].alt_text must contain 1..%d characters", i, maxArtifactAltTextRunes)
		}
	}
	return nil
}
