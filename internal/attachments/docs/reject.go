package docs

import "strings"

type RejectReason string

const RejectEmpty RejectReason = "empty"

func Reject(text string) RejectReason {
	if strings.TrimSpace(text) == "" {
		return RejectEmpty
	}
	return ""
}
