package http

import "strings"

func betLink(baseURL, betID string) string {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return "/bets/" + betID
	}
	return base + "/bets/" + betID
}
