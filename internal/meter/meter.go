// Package meter parses token usage out of provider responses as they stream
// through the proxy. Parsers are fed raw body bytes and never buffer more
// than they need; a parse failure never affects the proxied response.
package meter

import "mime"

type Usage struct {
	Model                    string
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// Parser consumes response body bytes as they pass through the proxy.
type Parser interface {
	Feed(p []byte)
	// Finish returns the usage gathered so far; ok reports whether any
	// usage information was found.
	Finish() (u Usage, ok bool)
}

// ForResponse returns a parser for an Anthropic response body based on its
// Content-Type, or nil when the content type carries no parseable usage.
func ForResponse(contentType string) Parser {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil
	}
	switch mediaType {
	case "application/json":
		return &jsonParser{}
	case "text/event-stream":
		return &sseParser{}
	default:
		return nil
	}
}
