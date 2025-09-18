package app

import (
    "regexp"
    "strings"
)

var markdownV2Escaper = strings.NewReplacer(
    "\\", "\\\\",
    "_", "\\_",
    "*", "\\*",
    "[", "\\[",
    "]", "\\]",
    "(", "\\(",
    ")", "\\)",
    "~", "\\~",
    "`", "\\`",
    ">", "\\>",
    "#", "\\#",
    "+", "\\+",
    "-", "\\-",
    "=", "\\=",
    "|", "\\|",
    "{", "\\{",
    "}", "\\}",
    ".", "\\.",
    "!", "\\!",
)

func escapeMarkdownV2(text string) string {
    return markdownV2Escaper.Replace(text)
}

var sentenceSplitter = regexp.MustCompile(`(?m)(?:\.|\?|!|\n)+`)

func summarizeThoughts(thoughts []string, limit int) []string {
    var cleaned []string
    for _, thought := range thoughts {
        for _, chunk := range sentenceSplitter.Split(thought, -1) {
            chunk = strings.TrimSpace(chunk)
            if chunk == "" {
                continue
            }
            cleaned = append(cleaned, chunk)
        }
    }
    if len(cleaned) > limit {
        cleaned = append(cleaned[:limit], "...")
    }
    return cleaned
}
