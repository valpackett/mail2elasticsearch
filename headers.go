package main

import (
	"regexp"
	"strings"
)

var addrSplitRegex = regexp.MustCompile(`\s*,\s*`)

func splitAddrs(vals []string) []string {
	result := make([]string, 0)
	for _, val := range vals {
		addrs := addrSplitRegex.Split(val, -1)
		for _, addr := range addrs {
			result = append(result, addr)
		}
	}
	return result
}

var whitespaceRegex = regexp.MustCompile(`\s+`)
var commentRegex = regexp.MustCompile(`\([^\)]*\)`)

// RFC 2822 allows whitespace and comments, ElasticSearch/joda-time does not
func stripSpaceAndComments(vals []string) []string {
	result := make([]string, 0)
	for _, val := range vals {
		val = commentRegex.ReplaceAllString(val, "")
		val = whitespaceRegex.ReplaceAllString(val, " ")
		val = strings.TrimSpace(val)
		result = append(result, val)
	}
	return result
}
