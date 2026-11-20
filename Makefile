.PHONY: fmt vet tidy test race check cover ci

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

test:
	go test ./...

race:
	go test ./... -race

# check：gofmt 格式检查，存在未格式化文件时报错退出并打印文件列表
# 说明：gofmt 在 Windows 下可能因 CRLF 换行符误报（core.autocrlf=true 时）；
# CI（Linux）环境无此问题，检查逻辑保持不变。若本地误报，可将
# core.autocrlf 设为 false 后重新检出，或先执行 `make fmt` 统一格式。
check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "以下文件未通过 gofmt 格式检查："; \
		echo "$$unformatted"; \
		echo "提示：Windows 下若因 CRLF 误报，可先执行 make fmt"; \
		exit 1; \
	else \
		echo "gofmt 检查通过"; \
	fi

# cover：运行测试并生成覆盖率报告，打印函数级覆盖率汇总
cover:
	go test ./... -coverprofile=coverage.out -count=1
	go tool cover -func=coverage.out

# ci：本地模拟 CI 流程，依次执行格式检查、静态检查、测试与竞态检测
ci: check vet test race
