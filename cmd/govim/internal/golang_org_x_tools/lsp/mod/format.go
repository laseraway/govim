package mod

import (
	"context"

	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/event"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/protocol"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/source"
)

func Format(ctx context.Context, snapshot source.Snapshot, fh source.FileHandle) ([]protocol.TextEdit, error) {
	ctx, done := event.Start(ctx, "mod.Format")
	defer done()

	pm, err := snapshot.ParseMod(ctx, fh)
	if err != nil {
		return nil, err
	}
	formatted, err := pm.File.Format()
	if err != nil {
		return nil, err
	}
	// Calculate the edits to be made due to the change.
	diff := snapshot.View().Options().ComputeEdits(fh.URI(), string(pm.Mapper.Content), string(formatted))
	return source.ToProtocolEdits(pm.Mapper, diff)
}
