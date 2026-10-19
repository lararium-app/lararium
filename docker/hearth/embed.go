// Package hearth embeds the persona seed templates that setup --defaults
// scaffolds, keeping one source of truth with the container image's defaults.
package hearth

import "embed"

// Templates contains the embedded persona seed files.
//
//go:embed SOUL.md IDENTITY.md USER.md MEMORY.md
var Templates embed.FS

// TemplateFiles lists the persona seed template filenames.
var TemplateFiles = []string{"SOUL.md", "IDENTITY.md", "USER.md", "MEMORY.md"}
