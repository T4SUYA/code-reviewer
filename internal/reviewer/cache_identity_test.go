package reviewer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OpticDiff/code-reviewer/internal/cache"
	"github.com/OpticDiff/code-reviewer/internal/config"
	"github.com/OpticDiff/code-reviewer/internal/model"
)

func TestReviewCacheModelID(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"model only", config.Config{Model: "m"}, "m"},
		{"unset thinking level is left out", config.Config{Model: "m", Profile: "security", Version: "1.0"}, "m:profile=security:version=1.0"},
		{"thinking level", config.Config{Model: "m", ThinkingLevel: "high"}, "m:thinking=high"},
		{"thinking level with profile and version", config.Config{Model: "m", Profile: "security", ThinkingLevel: "low", Version: "1.0"}, "m:profile=security:thinking=low:version=1.0"},
		{"consensus", config.Config{Models: []string{"a", "b"}, ConsensusThreshold: 2, ThinkingLevel: "medium"}, "consensus:a+b:threshold=2:thinking=medium"},
		{"api-url ignores the thinking level", config.Config{Model: "m", APIURL: "https://example.com/v1", ThinkingLevel: "high"}, "m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewCacheModelID(&tt.cfg); got != tt.want {
				t.Errorf("reviewCacheModelID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReviewCacheModelID_ThinkingLevelsDiffer(t *testing.T) {
	low := reviewCacheModelID(&config.Config{Model: "m", ThinkingLevel: "low"})
	high := reviewCacheModelID(&config.Config{Model: "m", ThinkingLevel: "high"})
	if low == high {
		t.Errorf("different thinking levels share cache identity %q", low)
	}
}

func TestReviewCacheModelID_PromptProfilesDiffer(t *testing.T) {
	ids := map[string]string{}
	for _, profile := range []string{"", "precision", "balanced", "recall"} {
		ids[profile] = reviewCacheModelID(&config.Config{Model: "m", PromptProfile: profile})
	}
	if ids[""] != ids["precision"] {
		t.Errorf("precision changes the identity: %q vs %q", ids[""], ids["precision"])
	}
	if ids["balanced"] == ids["recall"] || ids["balanced"] == ids["precision"] || ids["recall"] == ids["precision"] {
		t.Errorf("profiles share a cache identity: %v", ids)
	}
}

// A cached review is replayed only when it was produced under the same
// effective thinking level; otherwise the model is called again.
func TestRun_CacheReplayDependsOnEffectiveThinkingLevel(t *testing.T) {
	tests := []struct {
		name      string
		stored    config.Config // identity the cached entry was written under
		run       config.Config // settings of the re-run
		wantCalls int
	}{
		{"same level replays", config.Config{ThinkingLevel: "high"}, config.Config{ThinkingLevel: "high"}, 0},
		{"different level re-reviews", config.Config{ThinkingLevel: "low"}, config.Config{ThinkingLevel: "high"}, 1},
		{"level newly set re-reviews", config.Config{}, config.Config{ThinkingLevel: "high"}, 1},
		{"unset level keeps existing entries", config.Config{}, config.Config{}, 0},
		{"api-url ignores the level", config.Config{}, config.Config{ThinkingLevel: "high", APIURL: "https://example.com/v1"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diffs := makeTestDiffs("file1.go")
			cacheDir := t.TempDir()

			cfg := tt.run
			cfg.CacheDir = cacheDir
			cfg.CacheMaxAge = time.Hour
			cfg.Model = "test-model"
			cfg.ChunkStrategy = config.ChunkStrategyFail
			cfg.MinSeverity = config.SeverityLow

			stored := tt.stored
			stored.Model = cfg.Model
			storedID := reviewCacheModelID(&stored)

			c, err := cache.New(cacheDir, time.Hour)
			if err != nil {
				t.Fatalf("creating cache: %v", err)
			}
			promptHash := cache.PromptHash(cfg.CustomPrompt, "", "", cfg.Focus, cfg.ExtraRules, "")
			key := cache.CacheKey(cache.DiffHash(diffs[0]), storedID, promptHash)
			if err := c.Store(key, cache.Entry{FilePath: "file1.go", DiffHash: cache.DiffHash(diffs[0]), Model: storedID}); err != nil {
				t.Fatalf("storing cache entry: %v", err)
			}

			mm := &mockModel{result: &model.ReviewResult{Summary: "ok"}}
			r := NewWithDiffSource(&cfg, mm, &mockVCS{}, &mockDiffSource{diffs: diffs})
			if _, err := r.Run(context.Background()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mm.calls != tt.wantCalls {
				t.Errorf("model calls = %d, want %d", mm.calls, tt.wantCalls)
			}
		})
	}
}

// The configured prompt profile and confidence floor both reach the system
// prompt sent to the model.
func TestRun_PromptProfileAndFloorReachSystemPrompt(t *testing.T) {
	tests := []struct {
		profile string
		want    string
	}{
		{"", "When in doubt, DO NOT flag it"},
		{"balanced", "even if it is not certain"},
		{"recall", "Investigate every suspicious pattern"},
	}
	for _, tt := range tests {
		t.Run("profile="+tt.profile, func(t *testing.T) {
			cfg := config.Config{
				Model:           "test-model",
				PromptProfile:   tt.profile,
				CacheDir:        t.TempDir(),
				ConfidenceFloor: 55,
				ChunkStrategy:   config.ChunkStrategyFail,
				MinSeverity:     config.SeverityLow,
			}
			mm := &mockModel{result: &model.ReviewResult{Summary: "ok"}}
			r := NewWithDiffSource(&cfg, mm, &mockVCS{}, &mockDiffSource{diffs: makeTestDiffs("file1.go")})
			if _, err := r.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(mm.lastSystemPrompt, tt.want) {
				t.Errorf("system prompt missing %q", tt.want)
			}
			if !strings.Contains(mm.lastSystemPrompt, "confidence below 55%, omit the finding") {
				t.Error("confidence floor not applied")
			}
		})
	}
}
