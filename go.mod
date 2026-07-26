module github.com/PeterSR/claude-code-pigeon

go 1.25.8

require github.com/PeterSR/claude-code-weaverbird v0.0.0

// weaverbird's provider helper library is unpublished; wire it locally
// until it has a tagged release.
replace github.com/PeterSR/claude-code-weaverbird => /home/peter/dev/personal/claude-code-weaverbird
