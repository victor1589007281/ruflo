package tests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/anthropic/claude-go/pkg/vision"
)

func TestVisionLive_GenerateImage(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		apiKey = "sk-sp-b0a692b1b8384b72971fe4d3a42798a1"
	}

	client := vision.NewClient(apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	start := time.Now()
	result, err := client.GenerateImage(ctx, "一只可爱的橙色猫咪坐在窗台上看夕阳", "水彩画风格")
	elapsed := time.Since(start)
	fmt.Printf("耗时: %v\n", elapsed)
	if err != nil {
		t.Fatalf("GenerateImage 失败: %v", err)
	}

	fmt.Printf("=== 文生图测试结果 ===\n")
	fmt.Printf("SVG 长度: %d bytes\n", len(result.SVG))
	fmt.Printf("描述: %s\n", result.Desc)
	if len(result.SVG) > 300 {
		fmt.Printf("SVG 预览:\n%s...\n", result.SVG[:300])
	} else if result.SVG != "" {
		fmt.Printf("SVG:\n%s\n", result.SVG)
	}
	if result.SVG == "" {
		fmt.Printf("原始回复预览:\n%s\n", truncateStr(result.RawReply, 800))
	}

	if result.SVG == "" && result.RawReply == "" {
		t.Error("未生成任何内容")
	}
}

func TestVisionLive_TextCompletion(t *testing.T) {
	apiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" {
		apiKey = "sk-sp-b0a692b1b8384b72971fe4d3a42798a1"
	}

	client := vision.NewClient(apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	result, err := client.GenerateImage(ctx, "一个简单的笑脸图标", "极简设计")
	elapsed := time.Since(start)
	fmt.Printf("耗时: %v\n", elapsed)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	fmt.Printf("=== 极简图标测试 ===\n")
	fmt.Printf("SVG 长度: %d\n", len(result.SVG))
	if result.RawReply != "" {
		fmt.Printf("回复预览:\n%s\n", truncateStr(result.RawReply, 500))
	}
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
