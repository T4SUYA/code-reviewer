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
		{"vertex prompt too large", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "The input token count exceeds the maximum number of tokens allowed"}, false},
		{"vertex invalid argument", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "Thinking level XHIGH is not supported by this model"}, true},
		{"vertex unsupported schema", fmt.Errorf("generating content: %w", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "response schema is not supported"}), true},
		{"vertex failed precondition", genai.APIError{Code: 400, Status: "FAILED_PRECONDITION", Message: "Project is not allowed to use this model"}, true},
		{"http 400 invalid parameter", &httpStatusError{StatusCode: 400, Body: "unknown parameter: temperature"}, true},
		{"http 400 context length", &httpStatusError{StatusCode: 400, Body: "maximum context length is 8192 tokens"}, false},
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

func TestIsSplittable(t *testing.T) {
	tooLarge := genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "request is too large: input token count exceeds the limit"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bad answer", MarkBadAnswer(errors.New("parsing model response")), true},
		{"bad answer wrapped", fmt.Errorf("x: %w", MarkBadAnswer(errors.New("empty"))), true},
		{"vertex too large", tooLarge, true},
		{"vertex 413", genai.APIError{Code: 413}, true},
		{"http 413", &httpStatusError{StatusCode: 413}, true},
		{"http 400 length", &httpStatusError{StatusCode: 400, Body: "prompt is too long"}, true},
		{"rate limit", genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"}, false},
		{"http 429", &httpStatusError{StatusCode: 429}, false},
		{"quota", errors.New("quota exceeded for metric"), false},
		{"server error", genai.APIError{Code: 500}, false},
		{"unavailable", genai.APIError{Code: 503}, false},
		{"http 502", &httpStatusError{StatusCode: 502}, false},
		{"network", errors.New("HTTP request failed: connection reset by peer"), false},
		{"timeout", fmt.Errorf("generating content: %w", context.DeadlineExceeded), false},
		{"fatal is never splittable", MarkFatal(MarkBadAnswer(errors.New("x"))), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSplittable(tt.err); got != tt.want {
				t.Errorf("IsSplittable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestWithUsage_CarriesUsageThroughWrapping(t *testing.T) {
	u := &TokenUsage{TotalTokens: 42}
	err := fmt.Errorf("model review: %w", WithUsage(MarkBadAnswer(errors.New("bad")), u))
	if got := UsageOf(err); got == nil || got.TotalTokens != 42 {
		t.Errorf("UsageOf = %v, want the carried usage", got)
	}
	if !IsBadAnswer(err) {
		t.Error("the marker was lost")
	}
	if UsageOf(errors.New("x")) != nil || UsageOf(nil) != nil {
		t.Error("UsageOf invented usage")
	}
	if WithUsage(nil, u) != nil {
		t.Error("WithUsage(nil) != nil")
	}
	if e := errors.New("x"); WithUsage(e, nil) != e {
		t.Error("WithUsage without usage should return the error unchanged")
	}
}
