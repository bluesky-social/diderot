package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/attachments/docs"
	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// lentImages is the image fetcher of a connector that lends it to the caption
// worker while the connector runs. Its source's images wait, unclaimed, while
// none is lent, so they don't spend their failure budget.
type lentImages struct {
	settings.Live[lentFetcher]
}

// lentFetcher is a fetcher and the context of the run that lent it.
type lentFetcher struct {
	run     context.Context
	fetcher files.SourceFetcher
}

// Lend lends fetcher until the returned release, which waits for fetches in
// progress; they end when run does.
func (l *lentImages) Lend(run context.Context, fetcher files.SourceFetcher) (release func()) {
	l.Set(lentFetcher{run: run, fetcher: fetcher})
	return l.Clear
}

// FetchImage fetches through the lent fetcher, holding it lent until the
// fetch returns, and ends the fetch when the run that lent it ends. A fetch
// with nothing lent, or cut short by its run's end, fails transiently, so
// the image waits for the connector to run again.
func (l *lentImages) FetchImage(ctx context.Context, id, mediaType string) (data []byte, contentType string, err error) {
	if !l.With(func(lent lentFetcher) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(lent.run, cancel)()
		data, contentType, err = lent.fetcher.FetchImage(ctx, id, mediaType)
		if err != nil && lent.run.Err() != nil {
			err = &files.TransientError{Underlying: err}
		}
	}) {
		return nil, "", &files.TransientError{Underlying: errors.New("the connector is not running")}
	}
	return data, contentType, err
}

func (l *lentImages) Available() bool {
	_, ok := l.Get()
	return ok
}

// registerCaptionWorker captions images from every source with a fetcher.
func registerCaptionWorker(
	supervisor *worker.Supervisor, cfg *config.Config, models *inferenceModels, pool *pgxpool.Pool,
	mirror files.S3Mirror, logger *slog.Logger, imageFetchers map[string]files.SourceFetcher,
) {
	if mirror == nil || cfg.ImagesBucket == "" {
		logger.Info("attachment workers disabled (object storage unavailable)")
		return
	}
	// Agent session images are mirrored as their sessions arrive, by the
	// feed or a push, so they are read from the mirror whether or not the
	// session feed runs.
	imageFetchers["agent_session"] = files.NewAgentSessionFetcher(mirror)
	if !captionsEnabled(models) {
		logger.Error("caption worker disabled: needs a configured model registry provider serving chat")
		return
	}
	captioner, err := newCaptioner(models.caption)
	if err != nil {
		logger.Error("caption worker disabled", "err", err)
		return
	}
	captionWorker := &files.Worker{
		Pool: pool, S3: mirror, Captioner: captioner, Fetchers: imageFetchers,
		Logger: logger.With("component", "caption-worker"), Registry: prometheus.DefaultRegisterer,
		Config: files.Config{Concurrency: cfg.CaptionWorkers},
	}
	supervisor.AddFunc("caption", captionWorker.Run)
}

// registerDocumentWorker extracts the text of documentFetchers' sources'
// documents as the worker name. The Slack and Zendesk connectors each run
// one while their own Index documents setting is on; it returns the cleanup
// to run once the supervisor has stopped.
func registerDocumentWorker(
	supervisor *worker.Supervisor, name string, cfg *config.Config, models *inferenceModels, pool *pgxpool.Pool,
	mirror files.S3Mirror, logger *slog.Logger, gate docs.GateChecker, documentFetchers map[string]files.Fetcher,
) func() {
	cleanup := func() {}
	if mirror == nil || cfg.ImagesBucket == "" {
		logger.Warn("document ingest is on, but object storage is not configured; documents are not extracted")
		return cleanup
	}
	router := &docs.Router{}
	var pdfModel docs.Extractor
	if !documentExtractionEnabled(models) {
		logger.Error("PDF model unavailable; local document processing remains enabled",
			"err", "needs a configured model registry provider serving chat")
	} else if extractor, err := newDocumentExtractor(models.docExtract, logger.With("component", "doc-extractor")); err != nil {
		logger.Error("PDF model unavailable; local document processing remains enabled", "err", err)
	} else {
		pdfModel = extractor
	}
	parser, err := docs.NewPDFParser(docs.PDFParserConfig{
		Model: pdfModel, Instances: cfg.DocExtractWorkers, Logger: logger.With("component", "doc-pdf"),
	})
	if err != nil {
		logger.Error("PDF parser unavailable; office formats remain enabled", "err", err)
	} else {
		router.PDF = parser
		cleanup = func() {
			if err := parser.Close(); err != nil {
				logger.Warn("PDF parser close failed", "err", err)
			}
		}
	}
	documentWorker := &docs.Worker{
		Pool: pool, S3: mirror, Router: router, Fetchers: documentFetchers, Gate: gate,
		Logger: logger.With("component", "doc-worker"), Registry: prometheus.DefaultRegisterer,
		Config: docs.Config{Concurrency: cfg.DocExtractWorkers},
	}
	supervisor.AddFunc(name, documentWorker.Run)
	return cleanup
}

func newCaptioner(model boundModel) (*files.GatewayCaptioner, error) {
	return files.NewGatewayCaptioner(files.GatewayCaptionerConfig{
		BaseURL: model.endpoint.BaseURL, APIKey: model.endpoint.APIKey, Auth: model.endpoint.Auth, Headers: model.endpoint.Headers,
		ModelID: model.ID, TokenParam: model.ChatTokenParam,
		ReasoningTokenAllowance: model.ChatReasoningTokenAllowance,
		OmitTemperature:         model.ChatOmitTemperature, ExtraBody: model.ChatFields(),
	})
}

func newDocumentExtractor(model boundModel, logger *slog.Logger) (*docs.GatewayExtractor, error) {
	return docs.NewGatewayExtractor(docs.GatewayExtractorConfig{
		BaseURL: model.endpoint.BaseURL, APIKey: model.endpoint.APIKey, Auth: model.endpoint.Auth, Headers: model.endpoint.Headers,
		ModelID: model.ID, TokenParam: model.ChatTokenParam,
		ReasoningTokenAllowance: model.ChatReasoningTokenAllowance, ExtraBody: model.ChatFields(),
	}, logger)
}
