package extract

import (
	"regexp"
	"strings"
	"unicode"
)

var iso4217Re = regexp.MustCompile(`^[A-Z]{3}$`)

var currencyAliases = map[string]string{
	"€": "EUR", "EUR": "EUR", "EURO": "EUR", "EUROS": "EUR",
	"$": "USD", "US$": "USD", "USD": "USD",
	"£": "GBP", "GBP": "GBP",
	"CHF": "CHF", "FR": "CHF", "SFR": "CHF",
	"ZŁ": "PLN", "ZL": "PLN", "PLN": "PLN",
	"KČ": "CZK", "KC": "CZK", "CZK": "CZK",
}

// NormalizeCurrency maps a printed currency to ISO 4217.
func NormalizeCurrency(raw string) (string, bool) {
	key := strings.Map(func(r rune) rune {
		if r == '.' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.ToUpper(strings.TrimSpace(raw)))
	if c, ok := currencyAliases[key]; ok {
		return c, true
	}
	if iso4217Re.MatchString(key) {
		return key, true
	}
	return "", false
}

// CurrencyFromAmount extracts a currency from the non-numeric part of raw.
func CurrencyFromAmount(raw string) (string, bool) {
	rest := strings.Map(func(r rune) rune {
		switch {
		case isDigit(r), r == '.', r == ',', isApostrophe(r), isSpaceLike(r),
			r == '+', r == '-', r == '−', r == '–', r == '—', r == '(', r == ')':
			return -1
		}
		return r
	}, raw)
	return NormalizeCurrency(rest)
}
