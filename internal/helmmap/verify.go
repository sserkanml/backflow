package helmmap

import (
	"context"

	"github.com/sserkanml/backflow/internal/rendermap"
)

// Input is a proposal to verify: some fields of one resource, mapped to the
// value files of a Helm source.
type Input struct {
	App      rendermap.App
	Resource rendermap.ResourceKey
	Fields   []rendermap.Field
	Source   *Source
	// Files are the unmodified files of the render (CollectFiles).
	Files map[string][]byte
}

// Verify decides whether the drifted fields of a resource can be traced back to
// edits in value files (see rendermap.Verify).
func Verify(ctx context.Context, r rendermap.Renderer, in Input, opts rendermap.Options) (*rendermap.Outcome, error) {
	return rendermap.Verify(ctx, r, rendermap.Input{
		App: in.App, Resource: in.Resource, Fields: in.Fields, Files: in.Files,
		Provider: in.Source, Writer: ValuesWriter{},
	}, opts)
}
