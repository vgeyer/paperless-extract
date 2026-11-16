// Package extract turns document text into a validated total amount.
package extract

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/shopspring/decimal"
)

// ErrInvalidAmount is wrapped by every ParseAmount error.
var ErrInvalidAmount = errors.New("invalid amount")

func badAmount(msg string) (decimal.Decimal, error) {
	return decimal.Zero, fmt.Errorf("%w: %s", ErrInvalidAmount, msg)
}

func isAffix(r rune) bool {
	return unicode.IsLetter(r) || unicode.Is(unicode.Sc, r) || unicode.IsSpace(r) || r == ':' || r == '.'
}

func isSpaceLike(r rune) bool {
	return r == ' ' || r == ' ' || r == ' ' || r == ' '
}

func isApostrophe(r rune) bool { return r == '\'' || r == '’' }

func isDash(r rune) bool { return r == '-' || r == '–' || r == '—' }

// stripAffixes removes leading and trailing affix runs. A leading '.' or ','
// directly followed by a digit is kept.
func stripAffixes(s string) string {
	rs := []rune(s)
	i, j := 0, len(rs)
	for i < j && isAffix(rs[i]) {
		if (rs[i] == '.' || rs[i] == ',') && i+1 < j && rs[i+1] >= '0' && rs[i+1] <= '9' {
			break
		}
		i++
	}
	for j > i && isAffix(rs[j-1]) {
		j--
	}
	return string(rs[i:j])
}

// ParseAmount parses a total as printed into a decimal. Errors wrap ErrInvalidAmount.
func ParseAmount(raw string) (decimal.Decimal, error) {
	s := stripAffixes(raw)
	signs := 0
	neg := false

	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && len(s) >= 2 {
		neg = true
		signs++
		s = stripAffixes(s[1 : len(s)-1])
	}

	noDecimals := false
	rs := []rune(s)
	end := len(rs)
	for end > 0 && isDash(rs[end-1]) {
		end--
	}
	if end < len(rs) && end > 0 && (rs[end-1] == '.' || rs[end-1] == ',') {
		rs = rs[:end-1]
		noDecimals = true
	}
	s = string(rs)

	rs = []rune(s)
	if len(rs) > 0 && (rs[0] == '-' || rs[0] == '−' || rs[0] == '+') {
		if rs[0] != '+' {
			neg = true
		}
		signs++
		rs = rs[1:]
	}
	if len(rs) > 0 && (rs[len(rs)-1] == '-' || rs[len(rs)-1] == '−') {
		neg = true
		signs++
		rs = rs[:len(rs)-1]
	}
	if signs > 1 {
		return badAmount("multiple signs")
	}
	core := stripAffixes(string(rs))
	if core == "" {
		return badAmount("no digits")
	}

	d, err := parseCore([]rune(core), noDecimals)
	if err != nil {
		return decimal.Zero, err
	}
	if neg {
		d = d.Neg()
	}
	return d, nil
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func parseCore(rs []rune, noDecimals bool) (decimal.Decimal, error) {
	n := len(rs)
	if !isDigit(rs[0]) || !isDigit(rs[n-1]) {
		return badAmount("must start and end with a digit")
	}
	var dots, commas, others int
	lastDot, lastComma := -1, -1
	for i, r := range rs {
		switch {
		case isDigit(r):
		case r == '.':
			dots++
			lastDot = i
		case r == ',':
			commas++
			lastComma = i
		case isSpaceLike(r) || isApostrophe(r):
			others++
		default:
			return badAmount("unexpected character")
		}
		if !isDigit(r) && i > 0 && !isDigit(rs[i-1]) {
			return badAmount("adjacent separators")
		}
	}

	decPos := -1
	switch {
	case noDecimals:
	case dots > 0 && commas > 0:
		decPos = max(lastDot, lastComma)
		if (rs[decPos] == '.' && dots > 1) || (rs[decPos] == ',' && commas > 1) {
			return badAmount("ambiguous separators")
		}
	case dots+commas == 1:
		pos := max(lastDot, lastComma)
		k := 0
		for _, r := range rs[pos+1:] {
			if !isDigit(r) {
				break
			}
			k++
		}
		switch {
		case others > 0 || k != 3:
			decPos = pos
		default:
			intPart := rs[:pos]
			allZero := true
			for _, r := range intPart {
				if r != '0' {
					allZero = false
				}
			}
			if len(intPart) > 3 || allZero {
				decPos = pos
			}
		}
	}

	intRunes, frac := rs, []rune(nil)
	if decPos >= 0 {
		intRunes, frac = rs[:decPos], rs[decPos+1:]
		for _, r := range frac {
			if !isDigit(r) {
				return badAmount("separator after decimal separator")
			}
		}
	}

	// Thousands separators in the integer part: all the same rune, proper grouping.
	var sep rune
	groups := []int{0}
	var digits strings.Builder
	for _, r := range intRunes {
		if isDigit(r) {
			digits.WriteRune(r)
			groups[len(groups)-1]++
			continue
		}
		if sep == 0 {
			sep = r
		} else if sep != r {
			return badAmount("mixed thousands separators")
		}
		groups = append(groups, 0)
	}
	if len(groups) > 1 {
		if groups[0] < 1 || groups[0] > 3 {
			return badAmount("bad thousands grouping")
		}
		for _, g := range groups[1:] {
			if g != 3 {
				return badAmount("bad thousands grouping")
			}
		}
	}

	str := digits.String()
	if len(frac) > 0 {
		str += "." + string(frac)
	}
	d, err := decimal.NewFromString(str)
	if err != nil {
		return badAmount("not a number")
	}
	return d, nil
}

// AmountInText reports whether the amount in amountRaw occurs in text as a
// complete number, compared by value.
func AmountInText(amountRaw, text string) bool {
	want, err := ParseAmount(amountRaw)
	if err != nil {
		return false
	}
	want = want.Abs()

	rs := []rune(text)
	n := len(rs)
	digitsAt := func(i int) int { // number of consecutive digits starting at i
		k := 0
		for i+k < n && isDigit(rs[i+k]) {
			k++
		}
		return k
	}
	matches := func(c []rune) bool {
		d, err := ParseAmount(string(c))
		return err == nil && d.Equal(want)
	}

	for i := 0; i < n; {
		if !isDigit(rs[i]) {
			i++
			continue
		}
		start := i
		for i < n {
			if isDigit(rs[i]) {
				i++
				continue
			}
			r := rs[i]
			if r == '.' || r == ',' || isApostrophe(r) {
				if i+1 < n && isDigit(rs[i+1]) {
					i++
					continue
				}
			} else if isSpaceLike(r) && digitsAt(i+1) == 3 {
				i++
				continue
			}
			break
		}
		if matches(rs[start:i]) {
			return true
		}
	}
	return false
}
