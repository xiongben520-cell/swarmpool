package main

// diffusers 本地运行时：调用本地模型 HTTP 服务（对齐未来 ComfyUI 集成形态）
// 环境变量：SWARMPOOL_DIFFUSERS_URL（默认 http://127.0.0.1:18777）
// spark 不再包含任何子进程执行；模型服务由安装方（install.sh 下一迭代）托管。

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type diffusersInput struct {
	Prompt string `json:"prompt"`
	Seed   int64  `json:"seed"`
}

func runDiffusers(jobID string, input json.RawMessage) ([]map[string]any, error) {
	base := os.Getenv("SWARMPOOL_DIFFUSERS_URL")
	if base == "" {
		base = "http://127.0.0.1:18777"
	}
	var in diffusersInput
	if json.Unmarshal(input, &in) != nil || in.Prompt == "" {
		in.Prompt = "a colorful abstract painting"
	}
	if len(in.Prompt) > 2000 {
		return nil, fmt.Errorf("prompt too long")
	}
	if in.Seed <= 0 {
		in.Seed = 42
	}

	body, _ := json.Marshal(map[string]any{
		"prompt": in.Prompt,
		"seed":   in.Seed,
		"job_id": jobID,
	})
	url := base + "/generate"
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("diffusers service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("diffusers service %d: %s", resp.StatusCode, string(msg))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("artifact read failed: %w", err)
	}
	sum := sha256.Sum256(raw)
	return []map[string]any{{
		"name":        "out.png",
		"content_b64": base64.StdEncoding.EncodeToString(raw),
		"sha256":      hex.EncodeToString(sum[:]),
	}}, nil
}
