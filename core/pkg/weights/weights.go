// Package weights classifies a model repository's on-disk layout.
//
// It exists as its own package because two unrelated layers need the same
// answer for different reasons and neither owns it. The registry needs it to
// record what it fetched; the engine layer needs it to know what can be loaded.
// The failure this prevents is concrete: a GGUF repository has no config.json,
// so a check that only looks for config.json reports a llama.cpp model as
// ready and then fails to load it.
package weights

import (
	"path"
	"strings"
)

// Format is how a repository's bytes are laid out.
//
// A safetensors checkpoint and a GGUF file are not two files of the same model.
// They are the same model in two formats, only one of which any given engine
// can read.
type Format string

const (
	// Safetensors is the Hugging Face layout: config.json plus shards.
	Safetensors Format = "safetensors"
	// GGUF is llama.cpp's layout: one or a few .gguf files with the tokenizer
	// embedded. A separate tokenizer id is meaningless for it.
	GGUF Format = "gguf"
	// Unknown is a repository with no recognised weights in it. It is a
	// distinct value rather than an empty string so that "we do not know" can
	// never be read as "safetensors".
	Unknown Format = ""
)

// String makes the zero value legible in a status message instead of printing
// as nothing at all.
func (f Format) String() string {
	if f == Unknown {
		return "unknown"
	}
	return string(f)
}

// Of classifies a repository by the files it contains.
//
// Inferred rather than declared, because an operator pulling a repository
// should not have to know which format the community happens to publish for it.
// The rule is deliberately narrow: a .gguf file means GGUF, and a config.json
// means safetensors. Anything else is Unknown and stays that way, because a
// wrong guess here produces a model that looks deployable and is not.
func Of(files []string) Format {
	var hasGGUF, hasConfig bool
	for _, f := range files {
		switch {
		case strings.EqualFold(path.Ext(f), ".gguf"):
			hasGGUF = true
		case path.Base(f) == "config.json":
			hasConfig = true
		}
	}
	switch {
	case hasGGUF:
		return GGUF
	case hasConfig:
		return Safetensors
	default:
		return Unknown
	}
}

// Compatible reports whether an engine that loads want can load have.
//
// Kept as a method on the engine's own format rather than a table here, so
// that adding an engine is adding one line to its profile and not editing a
// matrix that will fall out of date.
func (f Format) Compatible(want Format) bool {
	if want == Unknown {
		// An engine that has declared nothing accepts nothing, rather than
		// silently accepting a format it was never checked against.
		return false
	}
	return f == want
}
