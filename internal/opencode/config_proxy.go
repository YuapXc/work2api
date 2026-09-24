package opencode

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func resolveProxyFiles(configPath string, cfg *Config) error {
	trimList(&cfg.Proxies)
	effective := append([]string(nil), cfg.Proxies...)
	if cfg.ProxyFile != "" {
		resolved := cfg.ProxyFile
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(configPath), resolved)
		}
		proxies, err := readProxyFile(resolved)
		if err != nil {
			return fmt.Errorf("load proxy file %s: %w", resolved, err)
		}
		effective = append(effective, proxies...)
	}
	effective = uniqueStrings(effective)
	if len(effective) == 0 {
		effective = []string{"direct"}
	}
	cfg.effectiveProxies = effective
	return nil
}

func readProxyFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var proxies []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		value := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), string(rune(0xFEFF))))
		value = strings.TrimSpace(stripProxyLineComment(value))
		if value != "" {
			proxies = append(proxies, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return proxies, nil
}

func stripProxyLineComment(line string) string {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
			continue
		}
		if line[i] == '#' || line[i] == ';' || (line[i] == '/' && i+1 < len(line) && line[i+1] == '/') {
			return line[:i]
		}
	}
	return line
}

func uniqueStrings(items []string) []string {
	out := items[:0]
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func trimList(items *[]string) {
	out := (*items)[:0]
	for _, item := range *items {
		if value := strings.TrimSpace(item); value != "" {
			out = append(out, value)
		}
	}
	*items = out
}

// stripJSONComments removes // and /* */ comments without changing newlines.
func stripJSONComments(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	lineComment := false
	blockComment := false
	for i := 0; i < len(data); i++ {
		current := data[i]
		if lineComment {
			if current == '\n' || current == '\r' {
				lineComment = false
				out = append(out, current)
			} else {
				out = append(out, ' ')
			}
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(data) && data[i+1] == '/' {
				out = append(out, ' ', ' ')
				i++
				blockComment = false
			} else if current == '\n' || current == '\r' {
				out = append(out, current)
			} else {
				out = append(out, ' ')
			}
			continue
		}
		if inString {
			out = append(out, current)
			if escaped {
				escaped = false
			} else if current == '\\' {
				escaped = true
			} else if current == '"' {
				inString = false
			}
			continue
		}
		switch {
		case current == '"':
			inString = true
			out = append(out, current)
		case current == '/' && i+1 < len(data) && data[i+1] == '/':
			lineComment = true
			out = append(out, ' ', ' ')
			i++
		case current == '/' && i+1 < len(data) && data[i+1] == '*':
			blockComment = true
			out = append(out, ' ', ' ')
			i++
		default:
			out = append(out, current)
		}
	}
	if blockComment {
		return nil, errors.New("unterminated block comment")
	}
	return out, nil
}
