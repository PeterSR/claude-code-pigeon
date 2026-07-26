package main

import (
	"io"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"

	"github.com/PeterSR/claude-code-pigeon/internal/pigeon"
)

// cmdWeaverbird is pigeon's whole status-line surface, and the only one:
// weaverbird owns the rendering, so pigeon reports state and says nothing
// about colour, width or where on the bar any of it lands.
//
// provider.Dispatch needs no cobra, so this is the whole handler: parse
// "spec"/"value" and any requested widget ids, hand off to internal/pigeon.
// Dispatch reads stdin itself and treats an interactive terminal as an empty
// session, so running this by hand at a prompt reports rather than hangs.
func cmdWeaverbird(args []string, stdin io.Reader, stdout io.Writer) error {
	return wb.Dispatch(args, stdin, stdout, pigeon.BuildWeaverbirdSpec(), pigeon.WeaverbirdValue)
}
