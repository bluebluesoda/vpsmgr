package pw

import (
	"regexp"
	"testing"
)

func TestURLSafe(t *testing.T) {
	re := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for i := 0; i < 50; i++ {
		s := URLSafe(10)
		if len(s) != 10 {
			t.Fatalf("URLSafe(10) = %q, want length 10", s)
		}
		if !re.MatchString(s) {
			t.Fatalf("URLSafe(10) = %q, contains non URL-safe char", s)
		}
	}
}
