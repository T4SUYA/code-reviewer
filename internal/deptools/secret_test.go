package deptools

import (
	"strings"
	"testing"
)

func TestLooksLikeSecret(t *testing.T) {
	secrets := []string{
		"ghp" + "_abcdefghijklmnopqrstuvwxyz0123456789",
		"github" + "_pat_11ABCDEFG0123456789_abcdefghijklmnop",
		"glpat" + "-abcdefghijklmnopqrst",
		"xox" + "b-1234567890-abcdefgh",
		"AKIA" + "IOSFODNN7EXAMPLE",
		"AI" + "za" + "SyA1234567890abcdefghijklmnopqrstuv",
		"sk-" + "proj-abcdefgh12345678",
		"eyJ" + "hbGciOiJIUzI1NiJ9.eyJ" + "zdWIiOiIxMjM0NTY3ODkwIn0",
		"-----" + "BEGIN PRIVATE KEY-----",
		"Bearer abcdef0123456789xyz",
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"q8Zr3Lk9Vb2Xw7Pd4Nm1Ts6Yc5Hf0JgA",
		strings.Repeat("a", 40),
	}
	for _, s := range secrets {
		if !looksLikeSecret(s) {
			t.Errorf("looksLikeSecret(%q) = false", s)
		}
	}
	harmless := []string{
		"react", "@typescript-eslint/eslint-plugin", "org.springframework.boot:spring-boot-starter-data-jpa",
		"github.com/modelcontextprotocol/go-sdk", "hashicorp/terraform-provider-google",
		"1.2.3-beta.1+build.5", "v0.0.0-20240101123456-abcdef123456", "22-alpine3.20", "7.46.1",
		"browserContext.storageState indexedDB", "kubernetes-client-api-extensions", "20240101123456",
		"Authorization header", "sk", "ghost", "bearer",
	}
	for _, s := range harmless {
		if looksLikeSecret(s) {
			t.Errorf("looksLikeSecret(%q) = true", s)
		}
	}
}
