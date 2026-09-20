package model

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Rust applies Unicode's Final_Sigma rule to the entire string. x/text limits
// its lookahead to 30 case-ignorable runes, so resolve sigmas before asking it
// for the remaining (including multi-rune) lowercase mappings.
func lowercaseIdentity(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	precededByCased := false
	for i, r := range s {
		if r == 'Σ' && precededByCased && !followedByCased(s[i+len("Σ"):]) {
			r = 'ς'
		}
		out.WriteRune(r)
		// Case_Ignorable takes precedence: some marks and modifier letters
		// are also Cased, but must not supply the context for a final sigma.
		if !isCaseIgnorable(r) {
			precededByCased = isCased(r)
		}
	}
	return cases.Lower(language.Und, cases.HandleFinalSigma(false)).String(out.String())
}

func followedByCased(s string) bool {
	for _, r := range s {
		if !isCaseIgnorable(r) {
			return isCased(r)
		}
	}
	return false
}

func isCased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Uppercase, unicode.Other_Lowercase)
}

func isCaseIgnorable(r rune) bool {
	// Unicode 17 Case_Ignorable: these categories plus the punctuation with
	// Word_Break = MidLetter, MidNumLet or Single_Quote.
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) {
		return true
	}
	switch r {
	case '\'', '.', ':', '\u00b7', '\u0387', '\u055f', '\u05f4', '\u2018', '\u2019',
		'\u2024', '\u2027', '\ufe13', '\ufe52', '\ufe55', '\uff07', '\uff0e', '\uff1a':
		return true
	}
	return false
}
