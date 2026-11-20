package ask

import (
	"context"
	"fmt"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/cloudwego/eino/schema"
)

var defaultPrompt = promptcatalog.MustEmbedded().MustText(promptcatalog.AskBase)

// DefaultPrompt is the Ask base instruction in the embedded Git-owned release.
// Live sessions resolve and pin a database-promoted release.
func DefaultPrompt() string { return defaultPrompt }

// Wording is a deployment's organization-specific text in built-in tool
// descriptions. The zero value is neutral. The organization's name is not
// part of it: descriptions carry OrgNamePlaceholder, which each run fills.
type Wording struct {
	// Code describes the Sourcebot index behind code_search and code_read.
	Code CodeIndex
}

// BuiltinToolInfos returns the native Eino metadata for every deploy-managed
// tool that may be registered with the Ask runner. Connector settings still
// decide which optional tools a run offers; this catalog lets operator
// surfaces review the same model-facing descriptions and schemas without live
// clients. wording is as a run would see it now; web_search's description
// keeps OrgNamePlaceholder, which only a run fills. sources are the
// registered sources the corpus tools filter by.
func BuiltinToolInfos(ctx context.Context, wording Wording, sources source.Infos) ([]*schema.ToolInfo, error) {
	code := func() (CodeClient, CodeIndex) { return nil, wording.Code }
	registrations := []RegisteredTool{
		NewEinoRetrieveTool(nil, sources),
		NewEinoListTool(nil, sources),
		NewEinoExpandTool(nil, nil, sources),
		NewEinoWebSearchTool(nil),
		NewEinoWebFetchTool(nil),
		NewEinoCodeSearchTool(code),
		NewEinoCodeReadTool(code),
	}
	infos := make([]*schema.ToolInfo, 0, len(registrations))
	for _, registration := range registrations {
		info, err := registration.Tool.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("ask tool catalog: %w", err)
		}
		if err := validateToolInfo(info); err != nil {
			return nil, fmt.Errorf("ask tool catalog: %w", err)
		}
		infos = append(infos, info)
	}
	return infos, nil
}
