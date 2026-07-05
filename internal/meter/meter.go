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

// ForResponse returns a usage parser for a provider's response body based
// on the provider type ("anthropic" or "openai") and the response
// Content-Type, or nil when the content type carries no parseable usage.
func ForResponse(providerType, contentType string) Parser {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil
	}
	openai := providerType == "openai"
	switch mediaType {
	case "application/json":
		if openai {
			return &openaiJSONParser{}
		}
		return &jsonParser{}
	case "text/event-stream":
		if openai {
			return newOpenAISSE()
		}
		return newAnthropicSSE()
	default:
		return nil
	}
}
