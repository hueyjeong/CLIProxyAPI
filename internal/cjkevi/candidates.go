package cjkevi

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clear simplified-Chinese forms absent from ordinary Japanese kanji. Shared
// Han characters alone are ambiguous and remain outside the candidate gate.
const simplifiedOnlyHanInJapanese = "这简们吗说对语门问进过东广业电长应开风爱汉买卖听读写药饭钱观区网务点选设线处复计变统权压检证厂历产请确认"

type candidate struct {
	Text    string
	Context string
	Script  string
	Offset  int
}

func candidates(text, language string) []candidate {
	var out []candidate
	offset := 0
	var fenceMark byte
	fenceLength := 0
	htmlComment := false
	quoteEnd := rune(0)
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !htmlComment && quoteEnd == 0 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			mark := trimmed[0]
			count := 0
			for count < len(trimmed) && trimmed[count] == mark {
				count++
			}
			if fenceMark == 0 && count >= 3 {
				fenceMark, fenceLength = mark, count
				offset += len(line)
				continue
			}
			if fenceMark == mark && count >= fenceLength && strings.TrimSpace(trimmed[count:]) == "" {
				fenceMark, fenceLength = 0, 0
				offset += len(line)
				continue
			}
		}
		if fenceMark != 0 || strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			offset += len(line)
			continue
		}
		inlineTicks := 0
		start := -1
		script := ""
		flush := func(end int) {
			if start >= 0 && end-start <= 256 && len(out) < 32 {
				before := line[:start]
				if boundary := strings.LastIndexAny(before, "`\"'“”「」『』"); boundary >= 0 {
					_, size := utf8.DecodeRuneInString(before[boundary:])
					before = before[boundary+size:]
				}
				if boundary := strings.LastIndex(before, "-->"); boundary >= 0 {
					before = before[boundary+3:]
				}
				after := line[end:]
				if boundary := strings.IndexAny(after, "`\"'“”「」『』"); boundary >= 0 {
					after = after[:boundary]
				}
				if boundary := strings.Index(after, "<!--"); boundary >= 0 {
					after = after[:boundary]
				}
				beforeRunes, afterRunes := []rune(before), []rune(after)
				out = append(out, candidate{
					Text:    line[start:end],
					Context: string(beforeRunes[max(0, len(beforeRunes)-24):]) + "<span>" + string(afterRunes[:min(len(afterRunes), 24)]),
					Script:  script,
					Offset:  offset + start,
				})
			}
			start = -1
			script = ""
		}
		for i := 0; i < len(line); {
			if htmlComment {
				if end := strings.Index(line[i:], "-->"); end >= 0 {
					i += end + 3
					htmlComment = false
					continue
				}
				break
			}
			if inlineTicks == 0 && quoteEnd == 0 && strings.HasPrefix(line[i:], "<!--") {
				flush(i)
				htmlComment = true
				i += 4
				continue
			}
			if line[i] == '`' && quoteEnd == 0 {
				count := 0
				for i+count < len(line) && line[i+count] == '`' {
					count++
				}
				if inlineTicks == 0 {
					flush(i)
					inlineTicks = count
				} else if count == inlineTicks {
					inlineTicks = 0
				}
				i += count
				continue
			}
			r, size := utf8.DecodeRuneInString(line[i:])
			if inlineTicks != 0 {
				i += size
				continue
			}
			if r == '\'' {
				previous, _ := utf8.DecodeLastRuneInString(line[:i])
				next, _ := utf8.DecodeRuneInString(line[i+size:])
				if unicode.In(previous, unicode.Latin) && unicode.In(next, unicode.Latin) {
					i += size
					continue
				}
			}
			if r == '”' || r == '」' || r == '』' {
				if quoteEnd == r {
					quoteEnd = 0
				}
				i += size
				continue
			}
			closeQuote := rune(0)
			switch r {
			case '"', '\'':
				closeQuote = r
			case '“':
				closeQuote = '”'
			case '「':
				closeQuote = '」'
			case '『':
				closeQuote = '』'
			}
			if closeQuote != 0 {
				if quoteEnd == 0 {
					flush(i)
					quoteEnd = closeQuote
				} else if quoteEnd == r {
					quoteEnd = 0
				}
				i += size
				continue
			}
			if quoteEnd == 0 {
				current := unexpectedScript(r, language)
				if current != script {
					flush(i)
					if current != "" {
						start = i
						script = current
					}
				}
			}
			i += size
		}
		flush(len(line))
		offset += len(line)
	}
	return out
}

func unexpectedScript(r rune, language string) string {
	switch {
	case unicode.In(r, unicode.Hangul):
		if language != "ko-KR" {
			return "hangul"
		}
	case unicode.In(r, unicode.Hiragana, unicode.Katakana):
		if language != "ja-JP" {
			return "kana"
		}
	case unicode.In(r, unicode.Han):
		if language != "zh-CN" && language != "zh-TW" && language != "ja-JP" {
			return "han"
		}
		if language == "ja-JP" && strings.ContainsRune(simplifiedOnlyHanInJapanese, r) {
			return "han"
		}
	}
	return ""
}

// HasUnexpectedScript gates structured JSON values without treating ordinary
// Han characters in Japanese or Chinese as language intrusions.
func HasUnexpectedScript(text, language string) bool {
	for _, r := range text {
		if unexpectedScript(r, language) != "" {
			return true
		}
	}
	return false
}
