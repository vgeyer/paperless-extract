package extract

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/template"
)

//go:embed prompt.tmpl
var defaultPrompt string

// DefaultPromptTemplate returns the embedded template.
func DefaultPromptTemplate() string { return defaultPrompt }

// PromptData is the data passed to the prompt template.
type PromptData struct {
	DefaultCurrency string
}

// Prompt is a parsed system prompt template.
type Prompt struct {
	t *template.Template
}

// ParsePrompt parses and test-executes a prompt template.
func ParsePrompt(tmpl string) (*Prompt, error) {
	t, err := template.New("prompt").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("parse prompt template: %w", err)
	}
	p := &Prompt{t: t}
	s, err := p.System(PromptData{DefaultCurrency: "EUR"})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("prompt template renders empty")
	}
	return p, nil
}

// LoadPrompt loads the template from path, or the built-in one if path is empty.
func LoadPrompt(path string) (*Prompt, error) {
	if path == "" {
		return ParsePrompt(DefaultPromptTemplate())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read prompt file: %w", err)
	}
	return ParsePrompt(string(b))
}

// System renders the system message.
func (p *Prompt) System(data PromptData) (string, error) {
	var sb strings.Builder
	if err := p.t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("render prompt template: %w", err)
	}
	return sb.String(), nil
}
