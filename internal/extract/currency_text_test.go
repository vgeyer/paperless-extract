package extract_test

import (
	"testing"

	"github.com/vgeyer/paperless-extract/internal/extract"
)

func TestNormalizeCurrency(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"€", "EUR", true},
		{"EUR", "EUR", true},
		{"eur", "EUR", true},
		{" Eur ", "EUR", true},
		{"EURO", "EUR", true},
		{"Euro", "EUR", true},
		{"EUROS", "EUR", true},
		{"E U R", "EUR", true},
		{"$", "USD", true},
		{"US$", "USD", true},
		{"us$", "USD", true},
		{"USD", "USD", true},
		{"£", "GBP", true},
		{"GBP", "GBP", true},
		{"CHF", "CHF", true},
		{"Fr.", "CHF", true},
		{"FR", "CHF", true},
		{"SFr.", "CHF", true},
		{"sfr", "CHF", true},
		{"zł", "PLN", true},
		{"ZŁ", "PLN", true},
		{"zl", "PLN", true},
		{"PLN", "PLN", true},
		{"Kč", "CZK", true},
		{"KC", "CZK", true},
		{"CZK", "CZK", true},
		{"JPY", "JPY", true},
		{"sek", "SEK", true},
		{"", "", false},
		{"   ", "", false},
		{"KR", "", false},
		{"kr", "", false},
		{"¥", "", false},
		{"Dollar", "", false},
		{"EU", "", false},
		{"E1R", "", false},
	}
	for _, c := range cases {
		got, ok := extract.NormalizeCurrency(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeCurrency(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCurrencyFromAmount(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"€ 1.234,56", "EUR", true},
		{"Fr. 1'234.50", "CHF", true},
		{"1.234,56", "", false},
		{"1.234,56 EUR", "EUR", true},
		{"EUR1234.56", "EUR", true},
		{"1.234,56 €", "EUR", true},
		{"(1.234,56 €)", "EUR", true},
		{"-12,00 zł", "PLN", true},
		{"$1,234.56", "USD", true},
		{"CHF 50.–", "CHF", true},
		{"1 234,56 Kč", "CZK", true},
		{"1234.-", "", false},
		{"", "", false},
		{"12 kr", "", false},
	}
	for _, c := range cases {
		got, ok := extract.CurrencyFromAmount(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("CurrencyFromAmount(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAmountInText(t *testing.T) {
	cases := []struct {
		amount, text string
		want         bool
	}{
		// §8.3 table
		{"1.234,56", "Gesamt: 1.234,56 EUR", true},
		{"1234,56", "Gesamt: 1.234,56 EUR", true},
		{"1.234,56", "Summe 1 234,56", true},
		{"1 234,56", "Summe 1.234,56", true},
		{"1.234,56 €", "Total EUR1234.56", true},
		{"1'234.50", "Total CHF 1'234.50", true},
		{"1234.-", "Total 1234.- CHF", true},
		{"12,00", "Betrag 12,00", true},
		{"1.234,56", "Gesamt: 11.234,56", false},
		{"1.234,56", "Gesamt: 1.234,567", false},
		{"234,56", "Gesamt: 1.234,56", false},
		{"12", "Betrag 12,00", true},                // equal value
		{"120,00", "Menge 2 120,00", false},         // whole candidates only
		{"234,56", "Gesamt 1 234,56", false},        // no suffix candidate
		{"567", "Total 1 234 567", false},           // no suffix candidate
		{"234 567,00", "1 234 567,00", false},       // no suffix candidate
		{"-1.234,56", "Gutschrift -1.234,56", true}, // absolute value
		{"2.024,00", "Datum 12.03.2024", false},
		{"12.340,0", "Gesamt 1.234,00", false}, // decimal shifted
		{"1.234", "Gesamt 12,34", false},       // decimal shifted
		{"12,34", "Rechnungsnr 1234", false},   // decimal shifted
		{"12,34", "12;34", false},              // ';' is not a separator
		{"€", "anything 123", false},
		// additional
		{"", "123", false},
		{"1.234,56", "", false},
		{"1.234,56", "Pos 1\nGesamtbetrag\t1.234,56 €\nDanke", true},
		{"1.234,56", "Summe 1 234,56 EUR", true},
		{"1.234,56", "Summe 1 234,56 EUR", true},
		{"1.234,56", "Summe 1’234,56 EUR", true},
		{"1.234,56", "1.234,56", true},
		{"1.234,56", "Total:1.234,56", true},
		{"1.234,56", "Rechnung 4711 vom 01.02.2024, Gesamt 1.234,56", true},
		{"99,00", "Total 199,00", false},
		{"99,00", "Total 99,001", false},
		{"99,00", "Menge 3 Preis 99,00", true},
		{"1.234", "Total 1234", true},
		{"1.234,00", "Summe 1234", true},
		{"2.120,00", "Menge 2 120,00", true},
		{"12,34", "12-34", false},
		{"12,34", "12/34", false},
		{"12,34", "12:34 Uhr", false},
		{"1.234,56", "Summe 1..234,56", false},
		{"1.234,56", "Summe 1 234 56", false},
		{"abc", "abc 12", false},
		{"0,50", "Rabatt 0,5 %", true},
		{"5,00", "Rabatt 0,5 %", false},
		// a space is only dropped before exactly three digits
		{"1234", "Pos 12 34", false},
		{"1234", "Pos 1 2345", false},
		// digits separated by a space and two digits are not joined
		{"350", "3 50 EUR", false},
		{"-12,00", "Gutschrift -12,00", true},
		{"(1.234,56)", "Total (1.234,56)", true},
	}
	for _, c := range cases {
		if got := extract.AmountInText(c.amount, c.text); got != c.want {
			t.Errorf("AmountInText(%q, %q) = %v, want %v", c.amount, c.text, got, c.want)
		}
	}
}
