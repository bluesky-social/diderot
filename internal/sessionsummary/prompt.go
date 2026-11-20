// Package sessionsummary contains model-call and chunking primitives for
// native Bsky Session v1 ingestion. The former session-list worker is retired.
package sessionsummary

import (
	"context"
	"strings"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

const (
	// These pin the inputs of summaries stored before input digests were:
	// the SHA-256 of the session map and reduce prompt bodies those
	// summaries were generated with (promptcatalog.BodySHA256), and the
	// language and activity lists of their tool schema. A stored summary
	// with an empty input_digest is reused without inference only while the
	// active prompts and lists still match these exactly
	// (IsLegacyCacheCompatible). The prompt texts themselves are not kept:
	// the hashes are all that is compared.
	legacyMapPromptSHA256    = "5d4fcb1d30026eedf2212c124165a7628c386454345fc8d984062e32e33115df"
	legacyReducePromptSHA256 = "acec233a910f5745a0a388d8ae2a54736918d24900528c12ce833dd170c3000a"
	legacyLanguageEnum       = "go\x00python\x00typescript\x00javascript\x00rust\x00sql\x00bash\x00yaml\x00terraform\x00pulumi\x00dockerfile\x00html\x00css\x00markdown\x00json\x00protobuf"
	legacyActivityEnum       = "debug\x00implement\x00refactor\x00test\x00review\x00investigate\x00document\x00deploy\x00provision\x00configure\x00incident\x00design\x00backfill\x00cleanup"
)

// PromptReleaseReader supplies the active atomic prompt bundle to native
// session-change summarization.
type PromptReleaseReader interface {
	Current(context.Context) (*promptcatalog.Release, error)
}

// PromptBundle combines Git-owned instruction prose with typed schema enums.
// The enums remain Go data because they define tool input constraints rather
// than natural-language instructions.
type PromptBundle struct {
	MapSystem    string   `json:"map_system"`
	ReduceSystem string   `json:"reduce_system"`
	LanguageEnum []string `json:"language_enum"`
	ActivityEnum []string `json:"activity_enum"`
}

var languageEnum = []string{
	"go", "python", "typescript", "javascript", "rust", "sql",
	"bash", "yaml", "terraform", "pulumi", "dockerfile",
	"html", "css", "markdown", "json", "protobuf",
}

var activityEnum = []string{
	"debug", "implement", "refactor", "test", "review", "investigate",
	"document", "deploy", "provision", "configure", "incident", "design",
	"backfill", "cleanup",
}

// PromptBundleFromCatalog resolves the session map/reduce instructions from
// one atomic release. Callers must retain the release ID as provenance.
func PromptBundleFromCatalog(bundle promptcatalog.Bundle) (PromptBundle, error) {
	mapSystem, err := bundle.Text(promptcatalog.SummarySessionMap)
	if err != nil {
		return PromptBundle{}, err
	}
	reduceSystem, err := bundle.Text(promptcatalog.SummarySessionReduce)
	if err != nil {
		return PromptBundle{}, err
	}
	return PromptBundle{
		MapSystem:    mapSystem,
		ReduceSystem: reduceSystem,
		LanguageEnum: append([]string(nil), languageEnum...),
		ActivityEnum: append([]string(nil), activityEnum...),
	}, nil
}

// IsLegacyCacheCompatible reports whether bundle exactly matches the prompt
// and typed-schema inputs that produced legacy summary rows before input
// digests were persisted. It deliberately fails closed after any prompt or
// enum change, preventing an old summary from being relabeled as current.
func IsLegacyCacheCompatible(bundle PromptBundle) bool {
	return promptcatalog.BodySHA256(bundle.MapSystem) == legacyMapPromptSHA256 &&
		promptcatalog.BodySHA256(bundle.ReduceSystem) == legacyReducePromptSHA256 &&
		strings.Join(bundle.LanguageEnum, "\x00") == legacyLanguageEnum &&
		strings.Join(bundle.ActivityEnum, "\x00") == legacyActivityEnum
}

// EmbeddedPromptBundle is used by isolated tests and integration probes. Live
// workers resolve the active database release on each tick or summarize call.
var EmbeddedPromptBundle = func() PromptBundle {
	bundle, err := PromptBundleFromCatalog(promptcatalog.MustEmbedded())
	if err != nil {
		panic(err)
	}
	return bundle
}()
