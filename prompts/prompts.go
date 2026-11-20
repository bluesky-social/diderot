// Package prompts holds the example prompt catalog: every instruction
// Diderot sends to a model, written for any organization. A fresh database
// seeds it as prompt release 1; from then on the database is the
// only source, edited in the admin UI (docs/prompts.md). manifest.yaml maps each stable key to its Markdown file.
package prompts

import "embed"

// FS is the catalog rooted at this directory: manifest.yaml plus the
// Markdown files it lists.
//
//go:embed manifest.yaml ask connector reports session summary
var FS embed.FS
