package deptools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Prefixes of the answers docs_lookup gives when the service has nothing.
const (
	noDocsPrefix     = "No documentation found"
	noSnippetsPrefix = "No documentation snippets returned"
)

const docsContextPath = "/api/v2/context"

func docsLookupSpec(opts Options) Spec {
	str := map[string]any{"type": "string"}
	return Spec{
		Name:        ToolDocsLookup,
		Description: "Fetch version-pinned library documentation from Context7 for a short topic.",
		Usage: `Args: {"library": "/<org>/<project>", "version": "<pinned version, e.g. 1.64.1>", "topic": "<short topic>"}
The library is a Context7 id such as /microsoft/playwright or /hashicorp/terraform-provider-google. Take the version from pinned_version.
The topic is at most 80 characters of letters, digits, spaces and . _ - / (an identifier or option name, for example "browserContext.storageState indexedDB"). It is not free text and never code.
Returns up to 8 KiB of documentation snippets.`,
		InputSchema:      objectSchema(map[string]any{"library": str, "version": str, "topic": str}, "library", "version", "topic"),
		Timeout:          opts.RegistryTimeout * 2,
		Untrusted:        true,
		ExternalEvidence: true,
		Inconclusive: func(out string) bool {
			return strings.HasPrefix(out, noDocsPrefix) || strings.HasPrefix(out, noSnippetsPrefix)
		},
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			library, _ := stringArg(args, "library")
			version, _ := stringArg(args, "version")
			topic, _ := stringArg(args, "topic")
			return docsLookup(ctx, opts, library, version, topic)
		},
	}
}

// docsQuery builds the documentation request from validated parts. The query
// text is a fixed template around the topic.
func docsQuery(library, version, topic string) (libraryID, query string, err error) {
	if !docsLibrary.MatchString(library) || hasDotSegment(library) {
		return "", "", errors.New(`invalid library: use a Context7 id such as "/org/project"`)
	}
	if err := validateVersion(version); err != nil {
		return "", "", err
	}
	topic = strings.Join(strings.FieldsFunc(topic, func(r rune) bool { return r == ' ' }), " ")
	if !docsTopic.MatchString(topic) || !strings.ContainsAny(topic, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
		return "", "", errors.New("invalid topic: use at most 80 characters of letters, digits, spaces and . _ - /")
	}
	if looksLikeSecret(topic) || looksLikeSecret(library) {
		return "", "", errSecretShaped("topic")
	}
	if version[0] >= '0' && version[0] <= '9' {
		version = "v" + version
	}
	return library + "/" + version, fmt.Sprintf("%s (%s %s)", topic, strings.TrimPrefix(library, "/"), version), nil
}

func docsLookup(ctx context.Context, opts Options, library, version, topic string) (string, error) {
	libraryID, query, err := docsQuery(library, version, topic)
	if err != nil {
		return "", err
	}
	base, err := parseBaseURL(opts.DocsURL)
	if err != nil {
		return "", fmt.Errorf("docs URL: %w", err)
	}
	rl := registryLookup{opts: opts}
	if key := opts.docsAPIKey(); key != "" && isDefaultDocsHost(base) {
		rl.authorization = "Bearer " + key
	}
	slog.Info("agent tool docs_lookup", "library", library, "version", version, "topic", topic, "host", base.Host, "api_key_sent", rl.authorization != "")

	params := url.Values{"libraryId": {libraryID}, "query": {query}, "type": {"txt"}}
	status, _, body, err := rl.do(ctx, base, http.MethodGet, docsContextPath+"?"+params.Encode(), "text/plain")
	if err != nil {
		return "", fmt.Errorf("lookup failed (result unknown, not evidence that the API does not exist): %w", err)
	}
	switch {
	case status == http.StatusNotFound:
		return fmt.Sprintf(noDocsPrefix+" for %s. The library or that version may not be indexed; "+
			"this says nothing about whether the API exists.", libraryID), nil
	case status != http.StatusOK:
		return "", fmt.Errorf("lookup failed (result unknown, not evidence that the API does not exist): docs service answered HTTP %d", status)
	}
	text := stripControl(strings.TrimSpace(string(body)))
	if text == "" {
		return fmt.Sprintf(noSnippetsPrefix+" for %s and that topic.", libraryID), nil
	}
	return capUTF8(text, opts.DocsMaxOutputBytes), nil
}

// isDefaultDocsHost reports whether base is the default documentation
// service. The API key goes only there: a DocsURL override, even one the
// operator set, never receives it.
func isDefaultDocsHost(base *url.URL) bool {
	def, err := parseBaseURL(defaultDocsURL)
	return err == nil && base.Scheme == def.Scheme && strings.EqualFold(base.Host, def.Host)
}

// stripControl drops control characters other than newline and tab from
// documentation text, so it cannot carry terminal escapes or NULs.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}

// capUTF8 cuts s to at most n bytes on a rune boundary and marks the cut.
func capUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[... truncated at %d bytes ...]", n)
}
