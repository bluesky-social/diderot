// Package skills embeds the agent skill `diderot setup` installs.
package skills

import "embed"

// FS holds the skill under diderot/.
//
//go:embed diderot
var FS embed.FS

// Name is the skill's directory name.
const Name = "diderot"
