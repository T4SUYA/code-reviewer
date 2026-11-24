package model

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/genai"
)

func TestIsFatal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"marked fatal", MarkFatal(errors.New("boom")), true},
		{"marked fatal and wrapped", fmt.Errorf("chunk 2: %w", MarkFatal(errors.New("boom"))), true},
		{"vertex 401", fmt.Errorf("generating content: %w", genai.APIError{Code: 401}), true},
		{"vertex 403", genai.APIError{Code: 403}, true},
		{"vertex unknown model", genai.APIError{Code: 404, Status: "NOT_FOUND"}, true},
		{"vertex prompt too large", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT"}, false},
		{"vertex overloaded", genai.APIError{Code: 503}, false},
		{"vertex rate limit", genai.APIError{Code: 429}, false},
		{"http 401", &httpStatusError{StatusCode: 401}, true},
		{"http 404", fmt.Errorf("x: %w", &httpStatusError{StatusCode: 404}), true},
		{"http 413", &httpStatusError{StatusCode: 413}, false},
		{"http 500", &httpStatusError{StatusCode: 500}, false},
		{"joined with a fatal one", errors.Join(errors.New("a"), genai.APIError{Code: 401}), true},
		{"no credentials", errors.New("google: could not find default credentials"), true},
		{"unparseable response", errors.New("parsing model response: invalid character"), false},
		{"deadline of one call", context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsFatal(tt.err); got != tt.want {
				t.Errorf("IsFatal(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMarkFatal_KeepsTheCause(t *testing.T) {
	cause := errors.New("token refused")
	err := MarkFatal(cause)
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(%v, cause) = false", err)
	}
	if err.Error() != cause.Error() {
		t.Errorf("message = %q, want %q", err.Error(), cause.Error())
	}
	if MarkFatal(nil) != nil {
		t.Error("MarkFatal(nil) != nil")
	}
}
