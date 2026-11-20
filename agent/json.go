package agent

import (
	"errors"
	"strings"
)

// ErrNoJSON 表示输入中未找到 JSON 内容。
var ErrNoJSON = errors.New("agent: 输入中未找到 JSON")

// ExtractJSON 从模型输出中提取 JSON 子串。
//
// 模型即使被要求只输出 JSON，也可能输出 markdown 围栏（```json ... ```）
// 或附加解释性文本。本函数依次处理：
//
//  1. 剥掉 markdown 代码围栏；
//  2. 定位首个 '{' 或 '['，以及与之匹配的最后一个同类型括号，
//     截取中间部分作为 JSON 子串。
//
// 未找到任何 JSON 时返回 ErrNoJSON。
func ExtractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ErrNoJSON
	}

	// 剥掉 markdown 围栏：以 ``` 开头且以 ``` 结尾时取围栏内内容。
	if strings.HasPrefix(s, "```") {
		body := s[3:]
		// 跳过围栏行的语言标记（如 json）。
		if idx := strings.IndexByte(body, '\n'); idx >= 0 && strings.TrimSpace(body[:idx]) != "" {
			body = body[idx+1:]
		}
		if end := strings.LastIndex(body, "```"); end >= 0 {
			body = body[:end]
		}
		s = strings.TrimSpace(body)
		if s == "" {
			return "", ErrNoJSON
		}
	}

	open := strings.IndexAny(s, "{[")
	if open < 0 {
		return "", ErrNoJSON
	}
	openCh := s[open]
	closeCh := byte('}')
	if openCh == '[' {
		closeCh = ']'
	}
	closeIdx := strings.LastIndexByte(s, closeCh)
	if closeIdx <= open {
		return "", ErrNoJSON
	}
	return s[open : closeIdx+1], nil
}
