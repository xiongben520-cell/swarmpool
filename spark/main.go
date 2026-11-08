// spark: SwarmPool 节点客户端（P0 版）
// 命令: up / status / doctor / uninstall
// 安全边界（protocol/spec.md 第8、9节）：
//   - 只写 ~/.swarmpool 目录（密钥+缓存）
//   - 遥测白名单：GPU 型号/显存/带宽/任务统计（sim 运行时上报模拟值）
//   - 与平台只做出站连接（HTTPS + WSS）
// 运行时: SWARMPOOL_RUNTIME=sim（内置模拟产物）；runtime-comfy 容器在下一迭代接入
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var (
	apiBase    = envOr("SWARMPOOL_API", "https://swarmpool.duckdns.org")
	modelsFlag = envOr("SWARMPOOL_MODELS", "sim-model")
	runtimeKnd = envOr("SWARMPOOL_RUNTIME", "sim")
	nodeTier   = envIntOr("SWARMPOOL_TIER", 1)
)

type nodeKey struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

func main() {
	log.SetFlags(log.LstdFlags)
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "up":
		run()
	case "status":
		status()
	case "doctor":
		doctor()
	case "uninstall":
		uninstall()
	default:
		fmt.Println("用法: spark [up|status|doctor|uninstall]")
	}
}

// ---- 密钥管理（~/.swarmpool/node.key，600）----

func dataDir() string {
	u, err := user.Current()
	if err != nil {
		log.Fatal("无法获取用户目录: ", err)
	}
	return filepath.Join(u.HomeDir, ".swarmpool")
}

func loadOrCreateKey() nodeKey {
	path := filepath.Join(dataDir(), "node.key")
	if raw, err := os.ReadFile(path); err == nil && len(raw) == 64 {
		seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err == nil && len(seed) == ed25519.SeedSize {
			priv := ed25519.NewKeyFromSeed(seed)
			return nodeKey{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}
		}
	}
	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		log.Fatal("创建数据目录失败: ", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		log.Fatal("生成密钥失败: ", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(priv.Seed())), 0o600); err != nil {
		log.Fatal("保存密钥失败: ", err)
	}
	log.Printf("节点密钥已生成: %s", path)
	return nodeKey{Priv: priv, Pub: pub}
}

// ---- 注册 ----

func register(k nodeKey) string {
	body, _ := json.Marshal(map[string]any{
		"pubkey": base64.StdEncoding.EncodeToString(k.Pub),
		"hw": map[string]any{
			"runtime": runtimeKnd,
			"gpu":     map[string]any{"model": "sim", "vram_gb": 0},
		},
		"models": strings.Split(modelsFlag, ","),
	})
	resp, err := http.Post(apiBase+"/node/register", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		log.Fatalf("注册失败: err=%v status=%d", err, statusCodeOf(resp))
	}
	defer resp.Body.Close()
	var out struct {
		NodeID string `json:"node_id"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.NodeID == "" {
		log.Fatal("注册响应异常")
	}
	return out.NodeID
}

func statusCodeOf(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

// ---- 主循环：WSS 出站 + 心跳 + 领任务 ----

func run() {
	k := loadOrCreateKey()
	nodeID := register(k)
	log.Printf("已注册: %s (runtime=%s models=%s tier=%d)", nodeID, runtimeKnd, modelsFlag, nodeTier)

	for {
		if err := session(k, nodeID); err != nil {
			log.Printf("连接断开: %v，5s 后重连", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func session(k nodeKey, nodeID string) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ed25519.Sign(k.Priv, []byte("node-auth:"+ts))
	wsURL := strings.Replace(apiBase, "https://", "wss://", 1) + "/node/ws?" +
		url.Values{
			"pubkey": {base64.StdEncoding.EncodeToString(k.Pub)},
			"ts":     {ts},
			"sig":    {base64.StdEncoding.EncodeToString(sig)},
		}.Encode()

	log.Printf("连接 %s", wsURL)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	hello, _ := json.Marshal(map[string]any{
		"pubkey": base64.StdEncoding.EncodeToString(k.Pub),
		"models": strings.Split(modelsFlag, ","),
		"tier":   nodeTier,
	})
	if err := send(ws, "hello", hello); err != nil {
		return err
	}

	done := make(chan struct{})
	go func() { // 心跳
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				hb, _ := json.Marshal(map[string]any{
					"vram_free_gb": 0, "disk_free_gb": 10, "busy": 0,
				})
				if send(ws, "heartbeat", hb) != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	defer close(done)
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		var env struct {
			V       int             `json:"v"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case "welcome":
			log.Printf("已接入池子 (welcome)")
		case "lease.offer":
			var p struct {
				JobID string `json:"job_id"`
			}
			if json.Unmarshal(env.Payload, &p) == nil {
				acc, _ := json.Marshal(map[string]any{"job_id": p.JobID, "lease_id": p.JobID})
				if err := send(ws, "lease.accept", acc); err != nil {
					return err
				}
				log.Printf("接单: %s", p.JobID)
			}
		case "job.assign":
			var p struct {
				JobID string          `json:"job_id"`
				Type  string          `json:"type"`
				Model string          `json:"model"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(env.Payload, &p) != nil {
				continue
			}
			// 执行与上报不阻塞读循环
			go func(jobID, typ string, input json.RawMessage) {
				start := time.Now()
				arts, err := execute(typ, jobID, input)
				runtimeS := int(time.Since(start).Seconds())
				if err != nil {
					log.Printf("任务 %s 执行失败: %v", jobID, err)
					fail, _ := json.Marshal(map[string]any{"job_id": jobID, "code": "runtime_error", "reason": err.Error()})
					_ = send(ws, "job.fail", fail)
					return
				}
				res, _ := json.Marshal(map[string]any{
					"job_id": jobID, "runtime_s": runtimeS, "artifacts": arts,
				})
				if err := send(ws, "job.result", res); err != nil {
					log.Printf("任务 %s 回执失败: %v", jobID, err)
				} else {
					log.Printf("任务 %s 完成 (%.1fs)", jobID, time.Since(start).Seconds())
				}
			}(p.JobID, p.Type, p.Input)
		default:
			// 未知消息忽略（协议兼容规则）
		}
	}
}

func send(ws *websocket.Conn, typ string, payload json.RawMessage) error {
	env := map[string]any{"v": 1, "type": typ, "ts": time.Now().Unix(), "payload": payload}
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	b, _ := json.Marshal(env)
	return ws.WriteMessage(websocket.TextMessage, b)
}

// ---- 运行时 ----

func execute(typ, jobID string, input json.RawMessage) ([]map[string]any, error) {
	switch {
	case runtimeKnd == "sim" && typ == "image.gen":
		pngBytes, sha := simImage(jobID)
		return []map[string]any{{
			"name":        "out.png",
			"content_b64": base64.StdEncoding.EncodeToString(pngBytes),
			"sha256":      sha,
		}}, nil
	case runtimeKnd == "diffusers" && typ == "image.gen":
		return runDiffusers(jobID, input)
	default:
		return nil, fmt.Errorf("runtime %q 不支持任务类型 %q", runtimeKnd, typ)
	}
}

// simImage: 由 jobID 哈希确定性生成 256x256 占位 PNG（stdlib，零依赖）
// 返回值第二个为 **PNG 字节** 的 sha256（供平台 quick-checks 校验，对齐 spec 第10节）
func simImage(seedStr string) ([]byte, string) {
	seed := sha256.Sum256([]byte(seedStr)) // 驱动像素颜色的种子
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(int(seed[0]) + x),
				G: uint8(int(seed[1]) + y),
				B: uint8(int(seed[2]) ^ x),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, ""
	}
	out := buf.Bytes()
	pngSum := sha256.Sum256(out)
	return out, hex.EncodeToString(pngSum[:])
}

// ---- 辅助命令 ----

func status() {
	k, err := os.ReadFile(filepath.Join(dataDir(), "node.key"))
	if err != nil {
		fmt.Println("未部署（无节点密钥）")
		return
	}
	fmt.Println("密钥: OK (", string(k[:8]), "...)")
	resp, err := http.Get(apiBase + "/v1/pool/stats")
	if err != nil {
		fmt.Println("平台不可达:", apiBase)
		return
	}
	defer resp.Body.Close()
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Printf("平台: %s\n%s\n", apiBase, b)
}

func doctor() {
	resp, err := http.Get(apiBase + "/healthz")
	if err != nil {
		fmt.Println("[FAIL] 平台不可达:", apiBase, err)
		return
	}
	defer resp.Body.Close()
	fmt.Printf("[OK] 平台可达 (%s)\n", apiBase)
	if _, err := os.Stat(filepath.Join(dataDir(), "node.key")); err == nil {
		fmt.Println("[OK] 节点密钥存在")
	} else {
		fmt.Println("[--] 节点密钥不存在（spark up 时自动生成）")
	}
	fmt.Printf("[OK] runtime=%s models=%s\n", runtimeKnd, modelsFlag)
}

func uninstall() {
	fmt.Println("提示：本命令只清理 ~/.swarmpool；容器/进程请用部署方式对应命令停止。")
	if err := os.RemoveAll(dataDir()); err != nil {
		fmt.Println("清理失败:", err)
		return
	}
	fmt.Println("已删除", dataDir())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
