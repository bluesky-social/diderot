package notion

import "regexp"

// Notion file URLs are short-lived signed links. Keeping them in event bodies
// creates re-embedding churn on every signature rotation and leaves stale
// links in retrieval results. Images are tracked separately by stable block
// identity, so remove only the expiring destination while retaining labels.
var expiringNotionMarkdownURL = regexp.MustCompile(`(!?\[[^\]]*\])\(https://[^\s)]*(?:X-Amz-(?:Algorithm|Credential|Date|Expires|Signature)|x-id=GetObject)[^\s)]*\)`)

func stripExpiringNotionURLs(markdown string) string {
	return expiringNotionMarkdownURL.ReplaceAllString(markdown, `$1`)
}
