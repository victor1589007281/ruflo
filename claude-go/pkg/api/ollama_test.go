package api

import "testing"

func TestNewOllamaClientDefaults(t *testing.T) {
	c := NewOllamaClient("", "gemma4:26b-a4b-it-qat")
	if c.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("默认 baseURL 不符: %s", c.BaseURL)
	}
	if c.Model != "gemma4:26b-a4b-it-qat" {
		t.Fatalf("模型名 (含冒号) 应原样保留: %s", c.Model)
	}
	if c.APIKey == "" {
		t.Fatal("应有占位 APIKey")
	}
	if c.FirstTokenTimeout.Seconds() < 200 {
		t.Fatalf("本地模型应放宽首 token 超时, got %v", c.FirstTokenTimeout)
	}
}

func TestIsLocalEndpoint(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:11434/v1":  true,
		"http://127.0.0.1:11434/v1":  true,
		"http://[::1]:11434/v1":      true,
		"http://0.0.0.0:8080":        true,
		"https://api.anthropic.com":  false,
		"https://api.kimi.com/v1":    false,
		"":                           false,
		"http://localhost.evil.com":  false,
	}
	for in, want := range cases {
		if got := IsLocalEndpoint(in); got != want {
			t.Errorf("IsLocalEndpoint(%q) = %v, want %v", in, got, want)
		}
	}
}
