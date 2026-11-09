package ladder

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3/option"
)

// maxErrorBodyChars bounds the response body kept in an error message.
const maxErrorBodyChars = 1000

// HTTPStatusError is an error response from the endpoint, with its status and
// the start of its body.
type HTTPStatusError struct {
	Method     string
	URL        string
	StatusCode int
	Status     string
	// Body is the response body, cropped to maxErrorBodyChars characters.
	Body string
	// BodyShortened is true when Body was cropped.
	BodyShortened bool
}

func (err *HTTPStatusError) Error() string {
	body := strings.TrimSpace(err.Body)
	if body == "" {
		body = "(empty response body)"
	}
	if err.BodyShortened {
		body += " ... (response body shortened to " + fmt.Sprint(maxErrorBodyChars) + " characters)"
	}
	return fmt.Sprintf("%s %s answered HTTP %s: %s", err.Method, err.URL, err.Status, body)
}

// StatusErrorMiddleware turns every response with a status of 400 or above
// into an HTTPStatusError. The SDK's own error keeps only the body's "error"
// object and loses a plain-text body; this keeps the status and the body.
func StatusErrorMiddleware() option.RequestOption {
	return option.WithMiddleware(func(request *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		response, err := next(request)
		if err != nil || response.StatusCode < http.StatusBadRequest {
			return response, err
		}
		defer response.Body.Close()

		// UTF-8 needs at most 4 bytes per character.
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyChars*4+1))
		body, shortened := cropRunes(string(raw), maxErrorBodyChars)
		if len(raw) > maxErrorBodyChars*4 {
			shortened = true
		}
		if readErr != nil {
			body += fmt.Sprintf(" (reading the rest of the response body failed: %v)", readErr)
		}
		status := response.Status
		if status == "" {
			status = fmt.Sprintf("%d %s", response.StatusCode, http.StatusText(response.StatusCode))
		}
		return nil, &HTTPStatusError{
			Method:        request.Method,
			URL:           request.URL.Redacted(),
			StatusCode:    response.StatusCode,
			Status:        status,
			Body:          body,
			BodyShortened: shortened,
		}
	})
}

// cropRunes returns at most limit characters of text and whether it cropped.
func cropRunes(text string, limit int) (string, bool) {
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:limit]), true
}
