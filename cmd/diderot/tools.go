package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/asktool"
	"github.com/bluesky-social/diderot/internal/kagi"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/sourcebot"
	"github.com/bluesky-social/diderot/internal/tavily"
	"github.com/bluesky-social/diderot/internal/websearch"
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
// through the selected provider.
type webSearchTool struct{ tavily.Connector }

func (webSearchTool) Info() asktool.Info {
	return asktool.Info{Title: "Web search", Step: "web-search", Summary: "Lets Ask search the web."}
}

const (
	webProviderField = "provider"
	kagiAPIKeyField  = "kagi_api_key"
	kagiProvider     = "kagi"
)

func (w webSearchTool) Fields() []settings.Field {
	fields := w.Connector.Fields()
	for i := range fields {
		if fields[i].Key == tavily.FieldAPIKey {
			fields[i].Label = "Tavily API key"
			fields[i].Required = false
			fields[i].Help = "Starts with tvly-. Required when the provider is Tavily."
		}
		if fields[i].Key == tavily.FieldBaseURL {
			fields[i].Label = "Tavily API URL"
		}
	}
	return append([]settings.Field{
		{Key: webProviderField, Label: "Provider", Kind: settings.KindEnum, Default: tavily.ConnectorName,
			Options: []string{tavily.ConnectorName, kagiProvider}, Help: "Ask sends web searches and page fetches to this provider."},
		{Key: kagiAPIKeyField, Label: "Kagi API key", Kind: settings.KindSecret,
			Help: "Required when the provider is Kagi."},
	}, fields...)
}

func (w webSearchTool) SecretEnv(field string) string {
	if field == kagiAPIKeyField {
		return "DIDEROT_KAGI_API_KEY"
	}
	return settings.SecretEnv(w.Connector, field)
}

func (w webSearchTool) client(r settings.Resolved) ask.WebClient {
	if r.String(webProviderField) == kagiProvider {
		if key := r.Secret(kagiAPIKeyField); key != "" {
			return kagi.NewClient(key, w.HTTP)
		}
		return nil
	}
	if r.Secret(tavily.FieldAPIKey) == "" {
		return nil
	}
	return tavily.ClientFrom(r, w.HTTP)
}

func (w webSearchTool) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	if r.String(webProviderField) != kagiProvider {
		if r.Secret(tavily.FieldAPIKey) == "" {
			return settings.TestReport{Checks: []settings.Check{{
				Name: "Search", Detail: "Tavily needs an API key.",
				Remedy: r.SupplySecret(tavily.FieldAPIKey, "an API key from app.tavily.com") + ".",
			}}}
		}
		return w.Connector.Test(ctx, r)
	}
	check := settings.Check{Name: "Search"}
	client := w.client(r)
	if client == nil {
		check.Detail = "Kagi needs an API key."
		check.Remedy = r.SupplySecret(kagiAPIKeyField, "an API key from kagi.com/api/keys") + "."
	} else if _, err := client.Search(ctx, websearch.SearchParams{Query: "Kagi", MaxResults: 1}); err != nil {
		var status *kagi.HTTPError
		switch {
		case errors.As(err, &status) && status.Code == http.StatusUnauthorized:
			check.Detail = "Kagi rejected the API key."
			check.Remedy = r.SupplySecret(kagiAPIKeyField, "an API key from kagi.com/api/keys") + "."
		case status != nil && status.Code == http.StatusForbidden:
			check.Detail = "Kagi denied access from this server."
			check.Remedy = "Check the API key's access settings in Kagi."
		case status != nil && (status.Code == http.StatusTooManyRequests || status.Code == http.StatusPaymentRequired):
			check.Detail = "Kagi reported a usage limit or billing issue."
			check.Remedy = "Check billing and usage in the Kagi API portal."
		default:
			check.Detail = "Kagi did not complete a search."
			check.Remedy = "Check that this server can reach kagi.com and test again."
		}
	} else {
		check.Passed = true
		check.Detail = "Kagi answered a one-result search."
	}
	return settings.TestReport{Checks: []settings.Check{check}}
}

func (w webSearchTool) Tools(current asktool.Current) []ask.RegisteredTool {
	client := asktool.Built(current, w.client)
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
