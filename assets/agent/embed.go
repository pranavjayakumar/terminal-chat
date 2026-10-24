// Package agentassets holds the files tc ships into a personal agent's home:
// the code (CLAUDE.md), the default soul, and the skills.
package agentassets

import "embed"

//go:embed CLAUDE.md soul.md skills
var FS embed.FS
