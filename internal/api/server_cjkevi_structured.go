package api

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/cjkevi"
)

// structuredJSONText recognizes Messages text that presents a JSON document.
func structuredJSONText(text string) bool {
	trimmed := bytes.TrimSpace([]byte(text))
	return len(trimmed) != 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

// structuredCorrectionText changes exactly one JSON string value, never a key or
// the surrounding document. JSON syntax and unrelated byte spans are retained.
func structuredCorrectionText(text, language string) (string, func(string) (string, error), bool) {
	if !structuredJSONText(text) || !json.Valid([]byte(text)) {
		return "", nil, false
	}
	p := structuredLeafParser{data: []byte(text), language: language}
	p.space()
	if !p.value(0) || p.count != 1 {
		return "", nil, false
	}
	p.space()
	if p.pos != len(p.data) {
		return "", nil, false
	}
	start, end, original := p.start, p.end, p.candidate
	return original, func(revised string) (string, error) {
		encoded, err := json.Marshal(revised)
		if err != nil {
			return "", err
		}
		result := make([]byte, 0, len(text)+len(encoded)-(end-start))
		result = append(result, text[:start]...)
		result = append(result, encoded...)
		result = append(result, text[end:]...)
		if !json.Valid(result) {
			return "", errors.New("invalid structured replacement")
		}
		return string(result), nil
	}, true
}

type structuredLeafParser struct {
	data            []byte
	language        string
	pos, start, end int
	candidate       string
	count           int
}

func (p *structuredLeafParser) space() {
	for p.pos < len(p.data) && (p.data[p.pos] == ' ' || p.data[p.pos] == '\n' || p.data[p.pos] == '\r' || p.data[p.pos] == '\t') {
		p.pos++
	}
}

func (p *structuredLeafParser) quoted() (string, int, int, bool) {
	start := p.pos
	p.pos++
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case '\\':
			p.pos += 2
		case '"':
			p.pos++
			var value string
			if json.Unmarshal(p.data[start:p.pos], &value) != nil {
				return "", 0, 0, false
			}
			return value, start, p.pos, true
		default:
			p.pos++
		}
	}
	return "", 0, 0, false
}

func (p *structuredLeafParser) value(depth int) bool {
	if depth > 64 || p.pos >= len(p.data) {
		return false
	}
	switch p.data[p.pos] {
	case '"':
		value, start, end, ok := p.quoted()
		if !ok {
			return false
		}
		if cjkevi.HasUnexpectedScript(value, p.language) {
			p.count++
			p.start, p.end, p.candidate = start, end, value
		}
		return true
	case '{', '[':
		object := p.data[p.pos] == '{'
		closing := byte(']')
		if object {
			closing = '}'
		}
		p.pos++
		p.space()
		if p.pos < len(p.data) && p.data[p.pos] == closing {
			p.pos++
			return true
		}
		for {
			if object {
				if p.pos >= len(p.data) || p.data[p.pos] != '"' {
					return false
				}
				if _, _, _, ok := p.quoted(); !ok {
					return false
				}
				p.space()
				if p.pos >= len(p.data) || p.data[p.pos] != ':' {
					return false
				}
				p.pos++
				p.space()
			}
			if !p.value(depth + 1) {
				return false
			}
			p.space()
			if p.pos >= len(p.data) {
				return false
			}
			if p.data[p.pos] == closing {
				p.pos++
				return true
			}
			if p.data[p.pos] != ',' {
				return false
			}
			p.pos++
			p.space()
		}
	default:
		for p.pos < len(p.data) && p.data[p.pos] != ',' && p.data[p.pos] != '}' && p.data[p.pos] != ']' && p.data[p.pos] != ' ' && p.data[p.pos] != '\n' && p.data[p.pos] != '\r' && p.data[p.pos] != '\t' {
			p.pos++
		}
		return true // json.Valid has already validated the scalar.
	}
}
