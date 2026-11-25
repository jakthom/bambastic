package management

import (
	"strings"
	"unicode"
)

var spoolFinishes = []string{"high speed", "dual color", "basic", "matte", "silk", "glow", "marble", "metal", "sparkle", "galaxy", "translucent", "tough", "wood", "aero", "lite", "gradient", "hf"}

func splitProductName(material, name string) (subtype, color string) {
	rest := strings.TrimSpace(name)
	starts := false
	raw, mat := []rune(rest), []rune(strings.TrimSpace(material))
	word := func(r rune) bool { return r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
	for i := 0; len(mat) > 0 && i+len(mat) <= len(raw); i++ {
		end := i + len(mat)
		if i > 0 && word(raw[i-1]) || end < len(raw) && word(raw[end]) {
			continue
		}
		if strings.EqualFold(string(raw[i:end]), string(mat)) {
			starts = i == 0
			rest = strings.TrimSpace(string(raw[:i]) + " " + string(raw[end:]))
			break
		}
	}
	var words, colors, finishes []string
	for _, t := range strings.Fields(rest) {
		t = strings.Trim(t, "-–—,/|()[]:;")
		if strings.IndexFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			words = append(words, t)
		}
	}
	for i := 0; i < len(words); {
		match := 0
		for _, f := range spoolFinishes {
			parts := strings.Fields(f)
			if i+len(parts) > len(words) {
				continue
			}
			ok := true
			for j, word := range parts {
				ok = ok && strings.EqualFold(strings.TrimRight(words[i+j], "+"), word)
			}
			if ok {
				match = len(parts)
				break
			}
		}
		if match > 0 {
			finishes = append(finishes, words[i:i+match]...)
			i += match
		} else {
			colors = append(colors, words[i])
			i++
		}
	}
	if len(finishes) == 0 && starts {
		base := " " + strings.NewReplacer("+", " ", "-", " ").Replace(strings.ToLower(material)) + " "
		hasFinish := false
		for _, f := range spoolFinishes {
			hasFinish = hasFinish || strings.Contains(base, " "+f+" ")
		}
		if !hasFinish {
			return rest, ""
		}
	}
	return strings.Join(finishes, " "), strings.Join(colors, " ")
}
