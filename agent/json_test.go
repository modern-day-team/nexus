package agent

import (
	"errors"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{name: "纯 JSON 对象", in: `{"a":1}`, want: `{"a":1}`},
		{name: "纯 JSON 数组", in: `[1,2]`, want: `[1,2]`},
		{name: "json 围栏", in: "```json\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "裸围栏", in: "```\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "前后杂文本", in: "结果如下：\n{\"a\":{\"b\":2}}\n以上。", want: `{"a":{"b":2}}`},
		{name: "嵌套花括号", in: `x {"a":{"b":[1]}} y`, want: `{"a":{"b":[1]}}`},
		{name: "无 JSON", in: "没有任何大括号", wantErr: ErrNoJSON},
		{name: "空串", in: "   ", wantErr: ErrNoJSON},
		{name: "只有开括号", in: `{`, wantErr: ErrNoJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractJSON(tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ExtractJSON(%q) err = %v, want %v", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractJSON(%q) 意外出错: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ExtractJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
