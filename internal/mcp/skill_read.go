package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (f SkillFile) Verify(data []byte) error {
	if int64(len(data)) != f.Size {
		return fmt.Errorf("failed verification: %d bytes, listed %d", len(data), f.Size)
	}

	if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != f.Digest {
		return errors.New("failed verification: sha256 does not match the listing")
	}

	return nil
}

func (c *Client) ReadSkillFile(ctx context.Context, file SkillFile) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	request := mcp.ReadResourceRequest{}
	request.Params.URI = file.URI

	result, err := c.client.ReadResource(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("resources/read: %w", err)
	}

	for _, content := range result.Contents {
		data, matches, err := skillResourceData(content, file.URI)
		if !matches {
			continue
		}

		if err != nil {
			return nil, err
		}

		if err := file.Verify(data); err != nil {
			return nil, err
		}

		return data, nil
	}

	return nil, errors.New("resources/read returned no content for this URI")
}

func skillResourceData(content mcp.ResourceContents, uri string) ([]byte, bool, error) {
	switch value := content.(type) {
	case mcp.TextResourceContents:
		return []byte(value.Text), value.URI == uri, nil
	case *mcp.TextResourceContents:
		return []byte(value.Text), value.URI == uri, nil
	case mcp.BlobResourceContents:
		return skillBlobData(value, uri)
	case *mcp.BlobResourceContents:
		return skillBlobData(*value, uri)
	default:
		return nil, false, nil
	}
}

func skillBlobData(content mcp.BlobResourceContents, uri string) ([]byte, bool, error) {
	if content.URI != uri {
		return nil, false, nil
	}

	data, err := base64.StdEncoding.DecodeString(content.Blob)
	if err != nil {
		return nil, true, fmt.Errorf("decode blob: %w", err)
	}

	return data, true, nil
}
