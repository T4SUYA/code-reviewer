package deptools

import (
	"math"
	"regexp"
	"strings"
)

// secretPrefixes match the leading shape of common credentials and tokens.
var secretPrefixes = regexp.MustCompile(`(?i)(` + strings.Join([]string{
	`sk-[a-z0-9_-]{8,}`,                 // API keys of the sk- family
	`gh[pousr]_[a-z0-9]{16,}`,           // GitHub tokens
	`github` + `_pat_[a-z0-9_]{16,}`,    // GitHub fine-grained tokens
	`glpat-[a-z0-9_-]{12,}`,             // GitLab tokens
	`xox[abprs]-[a-z0-9-]{8,}`,          // Slack tokens
	`(?:akia|asia)[a-z0-9]{16}`,         // AWS access key ids
	`aiza[a-z0-9_-]{30,}`,               // Google API keys
	`eyj[a-z0-9_-]{8,}\.[a-z0-9_-]{8,}`, // JWTs
	`-----begin`,                        // PEM blocks
	`(?:bearer|basic)\s+[a-z0-9+/=_.-]{12,}`,
}, "|") + `)`)

var alnumRun = regexp.MustCompile(`[A-Za-z0-9]+`)

const (
	minSecretRun     = 20  // Shortest run of letters and digits tested for randomness.
	minRandomEntropy = 3.5 // Bits per character above which a mixed run reads as random.
	minOpaqueRun     = 40  // Any run this long is opaque, whatever its characters.
)

// looksLikeSecret reports whether s has the shape of a credential: a known
// token prefix, a long hex digest, or a long run of letters and digits that
// mixes digits with letters and is as random as a key. Package names, versions
// and doc topics are short and made of words, so they pass. The check cannot
// prove a string harmless; it stops an agent from echoing a key it read.
func looksLikeSecret(s string) bool {
	if secretPrefixes.MatchString(s) {
		return true
	}
	for _, run := range alnumRun.FindAllString(s, -1) {
		switch {
		case len(run) >= minOpaqueRun:
			return true
		case len(run) >= 32 && isHex(run):
			return true
		case len(run) >= minSecretRun && hasDigit(run) && hasLetter(run) && entropy(run) >= minRandomEntropy:
			return true
		}
	}
	return false
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }
func hasLetter(s string) bool {
	return strings.ContainsAny(strings.ToLower(s), "abcdefghijklmnopqrstuvwxyz")
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	counts := map[rune]float64{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len([]rune(s)))
	var h float64
	for _, c := range counts {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}
