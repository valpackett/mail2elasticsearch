package main

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestStripSpaceAndComments(t *testing.T) {
	assert.Equal(
		t,
		[]string{ "Thu, 13 Feb 1969 23:32 -0330" },
		stripSpaceAndComments([]string{ ` Thu,
      13
        Feb
          1969
      23:32
               -0330 (Newfoundland Time)` }),
	)
}
func TestSplitAddrs(t *testing.T) {
	assert.Equal(
		t,
		[]string{ "hello world <test@example.com>", "nice@me.me (test)", "hi@example.com" },
		splitAddrs([]string{ "hello world <test@example.com> ,	 nice@me.me (test),hi@example.com" }),
	)
}
