// Package sandbox embeds the agent contract so the served copy always
// matches the image it ships in.
package sandbox

import _ "embed"

// LLMs is the contents of llms.txt.
//
//go:embed llms.txt
var LLMs string
