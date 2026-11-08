package main

// HTTP 层：API Key 鉴权、任务创建/查询/产物下载、节点注册（protocol/spec.md 第3、6、12节）

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ctxKey int

const keyIDCtx ctxKey = 1

// ---- 鉴权 ----

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing bearer"})
			return
		}
		raw := strings.TrimPrefix(h, "Bearer ")
		if !strings.HasPrefix(raw, "sk-sp-") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "bad key format"})
			return
		}
		sum := sha256.Sum256([]byte(raw))
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var id, credits int64
		var status string
		err := s.pool.QueryRow(ctx,
			`SELECT id, credits, status FROM api_keys WHERE key_hash=$1`,
			hex.EncodeToString(sum[:])).Scan(&id, &credits, &status)
		if err != nil || status != "active" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid key"})
			return
		}
		if credits <= 0 {
			writeJSON(w, http.StatusPaymentRequired, map[string]any{"error": "insufficient credits"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), keyIDCtx, id)))
	}
}

func keyIDFrom(r *http.Request) int64 {
	if v, ok := r.Context().Value(keyIDCtx).(int64); ok {
		return v
	}
	return 0
}

// ---- 池状态/模型/Key 管理 ----

// healthz: 存活+DB连通探针
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "db": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "db": true})
}

// poolStats: 公开只读池状态（节点/队列）
func (s *server) poolStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()

	var nodesOnline, nodesTotal, jobsQueued, jobsDone int
	err := s.pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE status='online'),
		  COUNT(*),
		  (SELECT COUNT(*) FROM jobs WHERE status='queued'),
		  (SELECT COUNT(*) FROM jobs WHERE status='done')
		FROM nodes`).Scan(&nodesOnline, &nodesTotal, &jobsQueued, &jobsDone)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	rows, err := s.pool.Query(ctx, `SELECT gpu_tier, COUNT(*) FROM nodes GROUP BY gpu_tier ORDER BY gpu_tier`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	tiers := map[string]int{}
	for rows.Next() {
		var tier, n int
		if rows.Scan(&tier, &n) != nil {
			continue
		}
		tiers["tier_"+strconv.Itoa(tier)] = n
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": map[string]any{"online": nodesOnline, "total": nodesTotal, "by_gpu_tier": tiers},
		"jobs":  map[string]any{"queued": jobsQueued, "done": jobsDone},
	})
}

// listModels: 任务类型注册表（spec 第7节）
func (s *server) listModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT type, runtime_image, pricing, verifier, sandbox_profile
		FROM task_types WHERE enabled = true`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	type taskType struct {
		Type           string          `json:"type"`
		RuntimeImage   string          `json:"runtime_image"`
		Pricing        json.RawMessage `json:"pricing"`
		Verifier       string          `json:"verifier"`
		SandboxProfile string          `json:"sandbox_profile"`
	}
	out := []taskType{}
	for rows.Next() {
		var t taskType
		if err := rows.Scan(&t.Type, &t.RuntimeImage, &t.Pricing, &t.Verifier, &t.SandboxProfile); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_types": out})
}

// createKey: 管理端签发消费者 Key；明文只返回一次，库内存 sha256
func (s *server) createKey(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Admin-Token") != s.adminToken {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}
	raw := "sk-sp-" + randHex(24)
	sum := sha256.Sum256([]byte(raw))
	ctx, cancel := contextWithTimeout(r)
	defer cancel()
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO api_keys (key_hash, label) VALUES ($1, $2) RETURNING id`,
		hex.EncodeToString(sum[:]), r.URL.Query().Get("label")).Scan(&id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "key": raw, "credits": 500,
	})
}

// ---- 任务 ----

type createJobReq struct {
	Type    string          `json:"type"`
	Model   string          `json:"model"`
	Input   json.RawMessage `json:"input"`
	Webhook string          `json:"webhook"`
}

func (s *server) createJob(w http.ResponseWriter, r *http.Request) {
	var req createJobReq
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Type == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	ctx, cancel := contextWithTimeout(r)
	defer cancel()

	// 任务类型必须在注册表中且启用（spec 第7节）
	var pricing json.RawMessage
	err := s.pool.QueryRow(ctx,
		`SELECT pricing FROM task_types WHERE type=$1 AND enabled=true`, req.Type).Scan(&pricing)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown task type"})
		return
	}

	// MVP 估价值：最低 10 credits；实际结算按执行时长（spec 第11节）
	const estimate = int64(10)
	var credits int64
	if err := s.pool.QueryRow(ctx, `SELECT credits FROM api_keys WHERE id=$1`, keyIDFrom(r)).Scan(&credits); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if credits < estimate {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{"error": "insufficient credits"})
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"input":   req.Input,
		"webhook": req.Webhook,
	})
	jobID := "job_" + randHex(12)
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO jobs (id, api_key_id, type, model, payload, status) VALUES ($1,$2,$3,$4,$5::jsonb,'queued')`,
		jobID, keyIDFrom(r), req.Type, req.Model, string(payload)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.hub.Enqueue(jobID)

	writeJSON(w, http.StatusCreated, map[string]any{
		"id": jobID, "status": "queued", "estimate_credits": estimate,
	})
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()
	jobID := r.PathValue("id")

	var status, nodeID, failInfo, createdAt, finishedAt string
	var attempts, cost int64
	err := s.pool.QueryRow(ctx, `
		SELECT status, COALESCE(node_id,''), COALESCE(payload->>'fail',''),
		       attempts::bigint, credits_cost::bigint,
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SSZ"Z"'),
		       COALESCE(to_char(finished_at,'YYYY-MM-DD"T"HH24:MI:SSZ"Z"'),'')
		FROM jobs WHERE id=$1 AND api_key_id=$2`,
		jobID, keyIDFrom(r)).Scan(&status, &nodeID, &failInfo, &attempts, &cost, &createdAt, &finishedAt)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}

	rows, err := s.pool.Query(ctx, `
		SELECT storage_key, sha256, size_bytes, meta->>'name'
		FROM artifacts WHERE job_id=$1 ORDER BY id`, jobID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	type artMeta struct {
		Index int    `json:"index"`
		Name  string `json:"name"`
		SHA   string `json:"sha256"`
		Size  int64  `json:"size_bytes"`
	}
	arts := []artMeta{}
	for rows.Next() {
		var key, sha, name string
		var size int64
		if rows.Scan(&key, &sha, &size, &name) != nil {
			continue
		}
		idx := 0
		if i := strings.LastIndex(key, ":"); i >= 0 {
			idx, _ = strconv.Atoi(key[i+1:])
		}
		arts = append(arts, artMeta{Index: idx, Name: name, SHA: sha, Size: size})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": jobID, "status": status, "node_id": nodeID, "attempts": attempts,
		"credits_cost": cost, "fail": json.RawMessage(orNull(failInfo)),
		"artifacts": arts,
	})
}

func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()
	jobID := r.PathValue("id")
	n, _ := strconv.Atoi(r.PathValue("n"))

	// 归属校验
	var owner int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(api_key_id,0) FROM jobs WHERE id=$1`, jobID).Scan(&owner)
	if err != nil || owner != keyIDFrom(r) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}

	s.hub.artMu.RLock()
	arts := s.hub.artBank[jobID]
	s.hub.artMu.RUnlock()
	if n < 0 || n >= len(arts) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "artifact not found"})
		return
	}
	a := arts[n]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Artifact-Name", a.Name)
	w.Header().Set("X-Artifact-SHA256", a.SHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.Bytes)
}

// ---- 节点注册 ----

type registerReq struct {
	Pubkey string          `json:"pubkey"` // ed25519 公钥 base64(std)
	HW     json.RawMessage `json:"hw"`
	Models []string        `json:"models"`
}

func (s *server) registerNode(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	pk, err := base64.StdEncoding.DecodeString(req.Pubkey)
	if err != nil || len(pk) != ed25519.PublicKeySize {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad pubkey"})
		return
	}
	nodeID := nodeIDFromPubkey(ed25519.PublicKey(pk))
	ctx, cancel := contextWithTimeout(r)
	defer cancel()
	modelsJSON := mustJSON(orEmpty(req.Models))
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO nodes (id, pubkey, hw, models, status)
		VALUES ($1,$2,$3::jsonb,$4::jsonb,'offline')
		ON CONFLICT (id) DO UPDATE SET hw=$3::jsonb, models=$4::jsonb`,
		nodeID, req.Pubkey, orEmptyJSON(req.HW), modelsJSON); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("node registered: %s", nodeID)
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id": nodeID, "ws_path": "/node/ws",
	})
}

// ---- 工具 ----

func contextWithTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

func orEmpty(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func orEmptyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func orNull(s string) string {
	if s == "" {
		return "null"
	}
	return s
}

func init() {
	// 保留 log 前缀风格一致
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}
