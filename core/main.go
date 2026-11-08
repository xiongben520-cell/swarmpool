// swarmpool-core: P0 单体（API 网关 + 节点 WSS 枢纽 + 调度 + 积分账本）
// 对齐 protocol/spec.md：第5节 WSS 消息、第6节任务 Schema、第11节积分账本、第12节 HTTP API v1
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type server struct {
	pool       *pgxpool.Pool
	adminToken string
	hub        *Hub
}

func main() {
	dbURL := envOr("DATABASE_URL", "postgres://swarmpool:secret@localhost:5432/swarmpool")
	adminToken := envOr("ADMIN_TOKEN", "")
	if adminToken == "" {
		log.Fatal("ADMIN_TOKEN 必须设置（用于创建 API Key 的管理操作）")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("数据库 Ping 失败: %v", err)
	}

	s := &server{pool: pool, adminToken: adminToken}
	s.hub = NewHub(pool)
	go s.hub.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /v1/pool/stats", s.poolStats)
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("POST /v1/keys", s.createKey)
	mux.HandleFunc("POST /v1/jobs", s.auth(s.createJob))
	mux.HandleFunc("GET /v1/jobs/{id}", s.auth(s.getJob))
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts/{n}", s.auth(s.getArtifact))
	mux.HandleFunc("POST /node/register", s.registerNode)
	mux.HandleFunc("GET /node/ws", s.hub.handleWS)

	// 启动时把遗留 queued 任务重新入队
	go s.hub.RequeueExisting()

	addr := envOr("LISTEN_ADDR", ":8080")
	log.Printf("swarmpool-core listening on %s", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // WSS 长连接不受写超时约束，读超时由各 handler 管理
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Fatal(srv.ListenAndServe())
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	log.Printf("error: %v", err)
	writeJSON(w, code, map[string]any{"error": "internal"})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
