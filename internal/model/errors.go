package model

import (
	"errors"
	"net/http"
	"strings"

	"google.golang.org/genai"
)

// fatalError marks an error that no other request can recover from.
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

// MarkFatal marks err as one that will fail every model call the same way,
// such as a credential the provider refuses. IsFatal reports it. A nil error
// stays nil.
func MarkFatal(err error) error {
	if err == nil {
		return nil
	}
	return &fatalError{err: err}
}

// IsFatal reports whether err means the model cannot be used at all, however
// the request is shaped: the credentials are missing or refused, the caller is
// not permitted, or the model does not exist. A review stops at such an error
// instead of retrying smaller requests.
//
// Every other provider error, including an oversized prompt (400, 413), an
// overloaded service and an unparseable answer, may be specific to one
// request and is not fatal. A caller that must also stop on a cancelled run
// checks its own context.
func IsFatal(err error) bool {
	if err == nil {
		return false
	}
	var marked *fatalError
	if errors.As(err, &marked) {
		return true
	}
	if code, text, ok := apiStatus(err); ok {
		if fatalStatus(code) {
			return true
		}
		// A 400 that is not about the size of the request (an unsupported
		// schema, thinking level or temperature, a failed precondition)
		// fails every request the same way.
		if code == http.StatusBadRequest && !sizeMessage(text) {
			return true
		}
	}
	// Application Default Credentials that cannot be found surface as a plain
	// error from the auth library.
	return strings.Contains(strings.ToLower(err.Error()), "could not find default credentials")
}

// apiStatus extracts the HTTP status and the text of the server's reason from a
// Vertex AI or OpenAI-compatible provider error.
func apiStatus(err error) (code int, text string, ok bool) {
	var api genai.APIError
	if errors.As(err, &api) {
		return api.Code, api.Status + " " + api.Message, true
	}
	var apiPtr *genai.APIError
	if errors.As(err, &apiPtr) && apiPtr != nil {
		return apiPtr.Code, apiPtr.Status + " " + apiPtr.Message, true
	}
	var status *httpStatusError
	if errors.As(err, &status) {
		return status.StatusCode, status.Body, true
	}
	return 0, "", false
}

// sizeMessage reports whether the text of a 400 says the request was too big.
func sizeMessage(text string) bool {
	text = strings.ToLower(text)
	for _, w := range []string{"too long", "too large", "too big", "exceed", "token", "length", "size", "context window"} {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// IsRequestTooLarge reports whether err says the request exceeded what the
// model accepts: HTTP 413, or a 400 whose reason names size, length or tokens.
func IsRequestTooLarge(err error) bool {
	code, text, ok := apiStatus(err)
	if !ok {
		return false
	}
	return code == http.StatusRequestEntityTooLarge || (code == http.StatusBadRequest && sizeMessage(text))
}

// answerError marks an answer that came back but cannot be used.
type answerError struct{ err error }

func (e *answerError) Error() string { return e.err.Error() }
func (e *answerError) Unwrap() error { return e.err }

// MarkBadAnswer marks err as an answer the model gave that cannot be used: it
// is empty or does not parse. A smaller request may get a usable one.
func MarkBadAnswer(err error) error {
	if err == nil {
		return nil
	}
	return &answerError{err: err}
}

// IsBadAnswer reports whether err was marked with MarkBadAnswer.
func IsBadAnswer(err error) bool {
	var a *answerError
	return errors.As(err, &a)
}

// IsSplittable reports whether a smaller request can cure err: the answer was
// unusable or the request was too large. Rate limits, quota, server and
// network errors and timeouts are not: a smaller request meets them the same
// way. A fatal error is never splittable.
func IsSplittable(err error) bool {
	if err == nil || IsFatal(err) {
		return false
	}
	return IsBadAnswer(err) || IsRequestTooLarge(err)
}

// usageError carries the usage of a call that answered and then failed.
type usageError struct {
	err   error
	usage *TokenUsage
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// WithUsage attaches the usage of a call to the error it ended in, so a caller
// that counts tokens still sees the tokens of a call whose answer was unusable.
// It returns err unchanged when there is no usage.
func WithUsage(err error, usage *TokenUsage) error {
	if err == nil || usage == nil {
		return err
	}
	return &usageError{err: err, usage: usage}
}

// UsageOf returns the usage attached to err by WithUsage, or nil.
func UsageOf(err error) *TokenUsage {
	var u *usageError
	if errors.As(err, &u) {
		return u.usage
	}
	return nil
}

// fatalStatus reports the HTTP statuses that mean unauthenticated, forbidden or
// unknown model.
func fatalStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound
}
