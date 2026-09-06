package pw

import "regexp"

// MinLen is the minimum allowed length for a user-set password. The rule is
// shared by the user panel and the admin panel.
const MinLen = 10

var (
	hasLetter = regexp.MustCompile(`[A-Za-z]`)
	hasDigit  = regexp.MustCompile(`[0-9]`)
)

// RejectTooShort marks a password below MinLen.
const RejectTooShort = "too short"

// RejectWeak marks a password that meets the length rule but lacks a letter
// and a digit.
const RejectWeak = "weak"

// Validate enforces the shared password rule: at least MinLen characters,
// containing at least one letter and one digit. It returns "" when the
// password is acceptable, otherwise a stable reason constant for the caller
// to translate.
func Validate(p string) string {
	if len(p) < MinLen {
		return RejectTooShort
	}
	if !hasLetter.MatchString(p) || !hasDigit.MatchString(p) {
		return RejectWeak
	}
	return ""
}
