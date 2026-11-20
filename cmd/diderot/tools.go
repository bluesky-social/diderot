package main

import (
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/asktool"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/sourcebot"
	"github.com/bluesky-social/diderot/internal/tavily"
)

// connectorSettings is the settings store's cache as request paths read it:
// a connector's settings while it is enabled.
type connectorSettings interface {
	Connector(name string) (settings.Resolved, bool)
}

// connectorCache is the settings cache (*settings.Store): the connectors'
// settings and the tool connections.
type connectorCache interface {
	connectorSettings
	familyRows
}

// toolRegistry is every tool this binary gives Ask, in the order Ask lists
// their tools and pages list them after the sources.
func toolRegistry() asktool.Registry {
	return asktool.NewRegistry(webSearchTool{}, codeSearchTool{})
}

// webSearchTool is the web search connector: web_search and web_fetch
// through Tavily.
type webSearchTool struct{ tavily.Connector }

func (webSearchTool) Info() asktool.Info {
	return asktool.Info{Title: "Web search", Step: "web-search", Summary: "Lets Ask search the web."}
}

func (webSearchTool) Tools(current asktool.Current) []ask.RegisteredTool {
	client := asktool.Built(current, func(r settings.Resolved) ask.WebClient { return tavily.ClientFrom(r, nil) })
	web := func() ask.WebClient {
		c, _ := client()
		return c
	}
	return []ask.RegisteredTool{ask.NewEinoWebSearchTool(web), ask.NewEinoWebFetchTool(web)}
}

// codeSearchTool is the code search connector: code_search and code_read
// through Sourcebot, described with the index wording its fields give.
type codeSearchTool struct{ sourcebot.Connector }

func (codeSearchTool) Info() asktool.Info {
	return asktool.Info{Title: "Code search", Step: "code-search", Summary: "Lets Ask search and read your code."}
}

func (codeSearchTool) Tools(current asktool.Current) []ask.RegisteredTool {
	type codeIndex struct {
		client ask.CodeClient
		index  ask.CodeIndex
	}
	built := asktool.Built(current, func(r settings.Resolved) codeIndex {
		s := sourcebot.SettingsFrom(r)
		return codeIndex{client: sourcebot.NewClient(s.URL, s.APIKey, nil), index: codeIndexFrom(r)}
	})
	code := func() (ask.CodeClient, ask.CodeIndex) {
		c, _ := built()
		return c.client, c.index
	}
	return []ask.RegisteredTool{ask.NewEinoCodeSearchTool(code), ask.NewEinoCodeReadTool(code)}
}

// codeIndexFrom is what code search settings tell the model about the index.
func codeIndexFrom(r settings.Resolved) ask.CodeIndex {
	s := sourcebot.SettingsFrom(r)
	return ask.CodeIndex{RepoPrefix: s.RepoPrefix, SearchScope: s.SearchScope, ReadScope: s.ReadScope}
}

// toolWording is the organization wording in the built-in tool descriptions
// as a run starting now would see it.
func toolWording(store connectorSettings) func() ask.Wording {
	return func() ask.Wording {
		resolved, ok := store.Connector(sourcebot.ConnectorName)
		if !ok {
			return ask.Wording{}
		}
		return ask.Wording{Code: codeIndexFrom(resolved)}
	}
}
