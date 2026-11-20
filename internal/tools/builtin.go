// Package tools 提供内核内置工具。
package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

const (
	defaultFetchLimit = 64 * 1024
	fetchTimeout      = 15 * time.Second
)

// Builtins 返回内置工具集合。
func Builtins() ([]tool.BaseTool, error) {
	currentTime, err := newCurrentTimeTool()
	if err != nil {
		return nil, fmt.Errorf("tools: 构建 current_time 失败: %w", err)
	}
	httpFetch, err := newHTTPFetchTool()
	if err != nil {
		return nil, fmt.Errorf("tools: 构建 http_fetch 失败: %w", err)
	}
	return []tool.BaseTool{currentTime, httpFetch}, nil
}

type currentTimeInput struct {
	Timezone string `json:"timezone" jsonschema_description:"IANA 时区名，例如 Asia/Shanghai、America/New_York，留空则使用 UTC"`
}

func newCurrentTimeTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"current_time",
		"获取指定时区当前的日期与时间。",
		func(_ context.Context, in currentTimeInput) (string, error) {
			loc, err := time.LoadLocation(in.Timezone)
			if err != nil {
				return "", fmt.Errorf("无效的时区 %q: %w", in.Timezone, err)
			}
			now := time.Now().In(loc)
			return now.Format("2006-01-02 15:04:05 Monday (MST)"), nil
		},
	)
}

type httpFetchInput struct {
	URL      string `json:"url" jsonschema:"required" jsonschema_description:"需要抓取的 http/https 地址"`
	MaxBytes int    `json:"max_bytes" jsonschema_description:"最多读取的响应字节数，默认 65536，最大 262144"`
}

type httpFetchOutput struct {
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
	Truncated   bool   `json:"truncated"`
}

func newHTTPFetchTool() (tool.InvokableTool, error) {
	client := &http.Client{Timeout: fetchTimeout}

	return utils.InferTool(
		"http_fetch",
		"抓取一个 http/https 页面的文本内容，用于获取实时网页信息。",
		func(ctx context.Context, in httpFetchInput) (httpFetchOutput, error) {
			if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
				return httpFetchOutput{}, fmt.Errorf("仅支持 http/https 地址")
			}

			limit := in.MaxBytes
			switch {
			case limit <= 0:
				limit = defaultFetchLimit
			case limit > 4*defaultFetchLimit:
				limit = 4 * defaultFetchLimit
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
			if err != nil {
				return httpFetchOutput{}, err
			}
			req.Header.Set("User-Agent", "nexus-agent/0.1")

			resp, err := client.Do(req)
			if err != nil {
				return httpFetchOutput{}, err
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
			if err != nil {
				return httpFetchOutput{}, err
			}

			truncated := len(body) > limit
			if truncated {
				body = body[:limit]
			}

			return httpFetchOutput{
				StatusCode:  resp.StatusCode,
				ContentType: resp.Header.Get("Content-Type"),
				Body:        string(body),
				Truncated:   truncated,
			}, nil
		},
	)
}
