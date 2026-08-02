package api

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestExtractMCPContent(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent("hello"),
			mcp.NewImageContent("aGVsbG8=", "image/png"),
		},
	}

	text, blocks := extractMCPContent(result)

	if text != "hello" {
		t.Fatalf("expected text %q, got %q", "hello", text)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(blocks))
	}

	if blocks[0].Type != "text" || blocks[0].Text != "hello" {
		t.Fatalf("unexpected first block: %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Data != "aGVsbG8=" || blocks[1].MIMEType != "image/png" {
		t.Fatalf("unexpected image block: %+v", blocks[1])
	}
}

func TestExtractMCPContentImageOnly(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewImageContent("aGVsbG8=", "image/png"),
		},
	}

	text, blocks := extractMCPContent(result)

	if text != "" {
		t.Fatalf("expected empty text, got %q", text)
	}
	if len(blocks) != 1 || blocks[0].Type != "image" {
		t.Fatalf("expected single image block, got %+v", blocks)
	}
}

func TestExtractMCPContentEmpty(t *testing.T) {
	text, blocks := extractMCPContent(&mcp.CallToolResult{})

	if text != "" {
		t.Fatalf("expected empty text, got %q", text)
	}
	if len(blocks) != 0 {
		t.Fatalf("expected no blocks, got %+v", blocks)
	}
}
