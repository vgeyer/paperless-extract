package extract_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vgeyer/paperless-extract/internal/extract"
)

func validator() extract.Validator {
	return extract.Validator{MaxAmount: decimal.NewFromInt(1000000), DefaultCurrency: "EUR"}
}

func TestValidateAccepts(t *testing.T) {
	cases := []struct {
		name     string
		r        extract.Result
		text     string
		def      string
		amount   string
		currency string
	}{
		{"de euro sign", extract.Result{Found: true, AmountRaw: "1.234,56", Currency: "€", Evidence: "Gesamt 1.234,56 €"},
			"Rechnung\nGesamt 1.234,56 €\n", "EUR", "1234.56", "EUR"},
		{"currency code", extract.Result{Found: true, AmountRaw: "1'234.50", Currency: "chf"},
			"Total CHF 1'234.50", "EUR", "1234.5", "CHF"},
		{"currency from amount", extract.Result{Found: true, AmountRaw: "1 234,56 zł"},
			"Razem do zapłaty: 1 234,56 zł", "EUR", "1234.56", "PLN"},
		{"default currency", extract.Result{Found: true, AmountRaw: "12,00"},
			"Betrag 12,00", "EUR", "12", "EUR"},
		{"configured default currency", extract.Result{Found: true, AmountRaw: "12,00"},
			"Betrag 12,00", "PLN", "12", "PLN"},
		{"trailing zero beyond 2 decimals ok", extract.Result{Found: true, AmountRaw: "1234,500"},
			"Summe 1234,500", "EUR", "1234.5", "EUR"},
		{"dash suffix", extract.Result{Found: true, AmountRaw: "1234.-", Currency: "Fr."},
			"Total 1234.- CHF", "EUR", "1234", "CHF"},
		{"just below max", extract.Result{Found: true, AmountRaw: "999.999,99"},
			"Total 999.999,99", "EUR", "999999.99", "EUR"},
		{"one cent", extract.Result{Found: true, AmountRaw: "0,01"},
			"Total 0,01", "EUR", "0.01", "EUR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := validator()
			v.DefaultCurrency = c.def
			ex, err := v.Validate(c.r, c.text)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !ex.Amount.Equal(decimal.RequireFromString(c.amount)) {
				t.Errorf("Amount = %s, want %s", ex.Amount, c.amount)
			}
			if ex.Currency != c.currency {
				t.Errorf("Currency = %q, want %q", ex.Currency, c.currency)
			}
			if ex.AmountRaw != c.r.AmountRaw || ex.Evidence != c.r.Evidence {
				t.Errorf("raw/evidence not copied: %+v", ex)
			}
			if ex.Amount.Exponent() < -2 {
				t.Errorf("Amount %s has more than 2 decimals", ex.Amount)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		r    extract.Result
		text string
		want error
	}{
		{"not found", extract.Result{Found: false, AmountRaw: "12,00"}, "Betrag 12,00", extract.ErrNotFound},
		{"not found beats unparseable", extract.Result{Found: false, AmountRaw: "abc"}, "abc", extract.ErrNotFound},
		{"unparseable", extract.Result{Found: true, AmountRaw: "zwölf"}, "zwölf Euro", extract.ErrUnparseable},
		{"empty amount", extract.Result{Found: true}, "Betrag 12,00", extract.ErrUnparseable},
		{"unparseable beats not in text", extract.Result{Found: true, AmountRaw: "1.23.45"}, "nothing", extract.ErrUnparseable},
		{"not in text", extract.Result{Found: true, AmountRaw: "777,77"}, "Gesamt 1.234,56", extract.ErrNotInText},
		{"partial match", extract.Result{Found: true, AmountRaw: "234,56"}, "Gesamt 1.234,56", extract.ErrNotInText},
		{"negative", extract.Result{Found: true, AmountRaw: "-12,00"}, "Gutschrift -12,00", extract.ErrNotPositive},
		{"negative parens", extract.Result{Found: true, AmountRaw: "(12,00)"}, "Gutschrift (12,00)", extract.ErrNotPositive},
		{"zero", extract.Result{Found: true, AmountRaw: "0,00"}, "Summe 0,00", extract.ErrNotPositive},
		{"not in text beats negative", extract.Result{Found: true, AmountRaw: "-5,00"}, "Summe 4,00", extract.ErrNotInText},
		{"equal max", extract.Result{Found: true, AmountRaw: "1.000.000,00"}, "Total 1.000.000,00", extract.ErrTooLarge},
		{"above max", extract.Result{Found: true, AmountRaw: "2.500.000"}, "Total 2.500.000", extract.ErrTooLarge},
		{"too precise", extract.Result{Found: true, AmountRaw: "1234,567"}, "Total 1234,567", extract.ErrTooPrecise},
		{"too precise 4", extract.Result{Found: true, AmountRaw: "1,2345"}, "Total 1,2345", extract.ErrTooPrecise},
		{"negative beats too precise", extract.Result{Found: true, AmountRaw: "-1234,567"}, "Total -1234,567", extract.ErrNotPositive},
		{"unknown currency", extract.Result{Found: true, AmountRaw: "12,00", Currency: "kr"}, "Total 12,00 kr", extract.ErrCurrency},
		{"unknown currency symbol", extract.Result{Found: true, AmountRaw: "12,00", Currency: "¥"}, "Total ¥12,00", extract.ErrCurrency},
		{"ambiguous thousands then currency", extract.Result{Found: true, AmountRaw: "1,234", Currency: "kr"}, "x 1,234", extract.ErrCurrency},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := validator().Validate(c.r, c.text)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !errors.Is(err, extract.ErrRejected) {
				t.Errorf("err %v does not wrap ErrRejected", err)
			}
			if !errors.Is(err, c.want) {
				t.Errorf("err %v does not wrap %v", err, c.want)
			}
			for _, other := range []error{extract.ErrNotFound, extract.ErrUnparseable, extract.ErrNotInText,
				extract.ErrNotPositive, extract.ErrTooLarge, extract.ErrTooPrecise, extract.ErrCurrency} {
				if !errors.Is(other, c.want) && errors.Is(err, other) {
					t.Errorf("err %v also wraps %v", err, other)
				}
			}
			if c.r.AmountRaw != "" && len(c.r.AmountRaw) > 3 && strings.Contains(err.Error(), c.r.AmountRaw) {
				t.Errorf("error string leaks amount_raw: %q", err.Error())
			}
			if strings.Contains(err.Error(), c.text) {
				t.Errorf("error string leaks document text: %q", err.Error())
			}
		})
	}
}

func TestValidateCustomMaxAmount(t *testing.T) {
	v := extract.Validator{MaxAmount: decimal.NewFromInt(100), DefaultCurrency: "EUR"}
	if _, err := v.Validate(extract.Result{Found: true, AmountRaw: "99,99"}, "x 99,99"); err != nil {
		t.Errorf("99.99 < 100 rejected: %v", err)
	}
	if _, err := v.Validate(extract.Result{Found: true, AmountRaw: "100,00"}, "x 100,00"); !errors.Is(err, extract.ErrTooLarge) {
		t.Errorf("100 >= 100: %v", err)
	}
}

func TestValidateExplicitCurrencyWinsOverAmount(t *testing.T) {
	// r.Currency non-empty is normalised; CurrencyFromAmount only used when it is empty.
	ex, err := validator().Validate(extract.Result{Found: true, AmountRaw: "12,00 €", Currency: "CHF"}, "Total 12,00 €")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Currency != "CHF" {
		t.Errorf("Currency = %q, want CHF", ex.Currency)
	}
}
