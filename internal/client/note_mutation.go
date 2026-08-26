package client

import (
	"context"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func (c *Client) AppendNote(ctx context.Context, req protocol.AppendNoteRequest) (protocol.NoteMutationResponse, error) {
	req.Device = c.device
	if err := c.ensureCompatible(ctx); err != nil {
		return protocol.NoteMutationResponse{}, err
	}
	return postJSON[protocol.NoteMutationResponse](ctx, c, "/v1/note/append", req)
}

func (c *Client) EditNote(ctx context.Context, req protocol.EditNoteRequest) (protocol.NoteMutationResponse, error) {
	req.Device = c.device
	if err := c.ensureCompatible(ctx); err != nil {
		return protocol.NoteMutationResponse{}, err
	}
	return postJSON[protocol.NoteMutationResponse](ctx, c, "/v1/note/edit", req)
}
