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
	var api genai.APIError
	if errors.As(err, &api) && fatalStatus(api.Code) {
		return true
	}
	var apiPtr *genai.APIError
	if errors.As(err, &apiPtr) && apiPtr != nil && fatalStatus(apiPtr.Code) {
		return true
	}
	var status *httpStatusError
	if errors.As(err, &status) && fatalStatus(status.StatusCode) {
		return true
	}
	// Application Default Credentials that cannot be found surface as a plain
	// error from the auth library.
	return strings.Contains(strings.ToLower(err.Error()), "could not find default credentials")
}

// fatalStatus reports the HTTP statuses that mean unauthenticated, forbidden or
// unknown model.
func fatalStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound
}
