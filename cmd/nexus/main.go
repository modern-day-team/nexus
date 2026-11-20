package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modern-day-team/nexus/agent"
	"github.com/modern-day-team/nexus/internal/config"
	"github.com/modern-day-team/nexus/internal/tools"
	"github.com/modern-day-team/nexus/llm"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nexus:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	ctx := context.Background()

	chatModel, err := llm.NewOpenAI(ctx, llm.Config{
		APIKey:      cfg.APIKey,
		BaseURL:     cfg.BaseURL,
		Name:        cfg.ModelName,
		Temperature: cfg.Temperature,
		MaxTokens:   cfg.MaxTokens,
		Timeout:     cfg.Timeout,
	})
	if err != nil {
		return err
	}

	builtinTools, err := tools.Builtins()
	if err != nil {
		return err
	}

	a, err := agent.New(ctx, agent.Options{
		ChatModel:    chatModel,
		Tools:        builtinTools,
		SystemPrompt: cfg.SystemPrompt,
		MaxStep:      cfg.MaxStep,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Nexus agent kernel | model=%s | base_url=%s\n", cfg.ModelName, cfg.BaseURL)
	fmt.Println("输入 /help 查看命令，/exit 退出")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		fmt.Print("\nyou> ")
		if !scanner.Scan() {
			fmt.Println()
			return scanner.Err()
		}

		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		case "/help":
			fmt.Println("/help     查看命令\n/reset    清空会话历史\n/history  查看历史条数\n/exit     退出")
			continue
		case "/reset":
			a.Reset()
			fmt.Println("会话历史已清空")
			continue
		case "/history":
			fmt.Printf("当前历史 %d 条消息\n", len(a.History()))
			continue
		}

		fmt.Print("nexus> ")
		if err := streamChat(ctx, a, line); err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		}
		fmt.Println()
	}
}

func streamChat(ctx context.Context, a *agent.Agent, input string) error {
	stream, err := a.ChatStream(ctx, input)
	if err != nil {
		return err
	}
	defer stream.Close()

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Print(chunk.Content)
	}
}
