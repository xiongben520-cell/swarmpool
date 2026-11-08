package main

// 节点 WSS 枢纽与派发器（protocol/spec.md 第4、5节）
// 铁律：节点只出不进；本枢纽是唯一入站面，且只走签名认证的 WSS。

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

var errBadArtifact = errors.New("artifact invalid")

const (
	protoV              = 1
	leaseAcceptTimeout  = 10 * time.Second
	jobResultTimeout    = 180 * time.Second
	heartbeatIntervalS  = 15
	maxJobAttempts      = 3
	artifactMaxInlineKB = 512 // sim 阶段产物内联上限；R2 直传在下一迭代接入
)

type envelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	TS      int64           `json:"ts"`
	Sig     string          `json:"sig,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

type nodeConn struct {
	id     string
	pubkey ed25519.PublicKey
	models map[string]bool
	tier   int
	busy   int
	ws     *websocket.Conn
	wmu    sync.Mutex
}

type resultArtifact struct {
	Name       string `json:"name"`
	ContentB64 string `json:"content_b64"`
	SHA256     string `json:"sha256"`
}

type resultMsg struct {
	JobID    string            `json:"job_id"`
	OK       bool              `json:"ok"`
	Code     string            `json:"code,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	RuntimeS int               `json:"runtime_s,omitempty"`
	Artifacts []resultArtifact `json:"artifacts,omitempty"`
}

type Hub struct {
	pool *pgxpool.Pool

	mu    sync.RWMutex
	conns map[string]*nodeConn

	artMu   sync.RWMutex
	artBank map[string][]storedArtifact

	syncMu    sync.Mutex
	accepts   map[string]chan struct{}
	results   map[string]chan *resultMsg

	dispatchCh chan string
}

type storedArtifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size_bytes"`
	Bytes  []byte `json:"-"`
}

func NewHub(pool *pgxpool.Pool) *Hub {
	return &Hub{
		pool:       pool,
		conns:      map[string]*nodeConn{},
		artBank:    map[string][]storedArtifact{},
		accepts:    map[string]chan struct{}{},
		results:    map[string]chan *resultMsg{},
		dispatchCh: make(chan string, 256),
	}
}

// ---- 对外入口 ----

func (h *Hub) Enqueue(jobID string) {
	select {
	case h.dispatchCh <- jobID:
	default:
		log.Printf("dispatch queue full, drop %s", jobID)
	}
}

func (h *Hub) RequeueExisting() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := h.pool.Query(ctx, `SELECT id FROM jobs WHERE status='queued'`)
	if err != nil {
		log.Printf("requeue query: %v", err)
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		h.Enqueue(id)
		n++
	}
	if n > 0 {
		log.Printf("requeued %d legacy jobs", n)
	}
}

// ---- 派发器 ----

func (h *Hub) Run() {
	for jobID := range h.dispatchCh {
		h.dispatch(jobID)
	}
}

func (h *Hub) dispatch(jobID string) {
	ctx := context.Background()

	var jType, jModel, jPayload, jStatus string
	var apiKeyID int64
	var attempts int
	err := h.pool.QueryRow(ctx,
		`SELECT type, COALESCE(model,''), payload::text, status, attempts, COALESCE(api_key_id,0)
		 FROM jobs WHERE id=$1`, jobID).
		Scan(&jType, &jModel, &jPayload, &jStatus, &attempts, &apiKeyID)
	if err != nil {
		log.Printf("dispatch load %s: %v", jobID, err)
		return
	}
	if jStatus != "queued" {
		return
	}

	nc := h.pickNode(jModel)
	if nc == nil {
		// 无可用节点：留在队列，退避重试（MVP 无 TTL 截止，spec ttl_s 下一迭代接入）
		time.AfterFunc(5*time.Second, func() { h.Enqueue(jobID) })
		return
	}

	h.setStatus(ctx, jobID, "leased", nc.id)

	if !h.offerAndAssign(nc, jobID, jType, jModel, jPayload) {
		h.finishOrRetry(ctx, jobID, nc, apiKeyID, "no_response", "节点未响应派发")
		return
	}

	res := h.waitResult(jobID)
	if res == nil || !res.OK {
		reason := "节点执行失败"
		code := "node_fail"
		if res == nil {
			reason, code = "节点超时", "timeout"
		} else if res.Reason != "" {
			reason = res.Reason
			code = res.Code
		}
		h.finishOrRetry(ctx, jobID, nc, apiKeyID, code, reason)
		return
	}

	// ---- 结果验证（spec 第10节 quick-checks）----
	arts, err := h.storeArtifacts(ctx, jobID, res.Artifacts)
	if err != nil {
		h.finishOrRetry(ctx, jobID, nc, apiKeyID, "bad_artifact", "产物校验失败: "+err.Error())
		return
	}

	// ---- 记账（spec 第11节）：burn 需方 / mint 供方 ----
	cost := h.computeCost(ctx, nc.id, res.RuntimeS)
	if !h.burnAndMint(ctx, apiKeyID, nc.id, jobID, cost) {
		h.setJobFailed(ctx, jobID, "insufficient_credits", "需方积分不足")
		h.cleanupPending(jobID)
		return
	}

	h.setJobDone(ctx, jobID, cost)
	h.cleanupPending(jobID)
	log.Printf("job %s done on %s, cost=%d, artifacts=%d", jobID, nc.id, cost, len(arts))
}

func (h *Hub) pickNode(model string) *nodeConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var best *nodeConn
	for _, nc := range h.conns {
		if model != "" && !nc.models[model] {
			continue
		}
		if best == nil || nc.busy < best.busy || (nc.busy == best.busy && nc.tier > best.tier) {
			best = nc
		}
	}
	return best
}

func (h *Hub) offerAndAssign(nc *nodeConn, jobID, jType, jModel, jPayload string) bool {
	h.mu.Lock()
	nc.busy++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		nc.busy--
		h.mu.Unlock()
	}()

	acceptCh := h.registerAccept(jobID)
	h.registerResult(jobID) // 结果经 results[jobID] 通道交付，waitResult 消费
	// 注意：不能在这里 defer cleanupPending —— offerAndAssign 返回后 waitResult 还要用；
	// 清理由 dispatch 的各条出口路径（finishOrRetry/完成/积分不足）统一负责。

	// lease.offer
	offerPayload, _ := json.Marshal(map[string]any{
		"job_id": jobID, "lease_id": jobID, "ttl_s": int(leaseAcceptTimeout.Seconds()),
		"summary": map[string]string{"type": jType, "model": jModel},
	})
	if h.send(nc, "lease.offer", offerPayload) != nil {
		return false
	}
	select {
	case <-acceptCh:
	case <-time.After(leaseAcceptTimeout):
		return false
	}

	// job.assign：payload 里带任务输入（sim 阶段输入随消息内联；大资产走预签名URL，下一迭代）
	var wrap struct {
		Input json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal([]byte(jPayload), &wrap)
	assignPayload, _ := json.Marshal(map[string]any{
		"job_id": jobID, "type": jType, "model": jModel, "input": wrap.Input,
	})
	if h.send(nc, "job.assign", assignPayload) != nil {
		return false
	}
	return true
}

func (h *Hub) waitResult(jobID string) *resultMsg {
	h.syncMu.Lock()
	ch := h.results[jobID]
	h.syncMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case res := <-ch:
		return res
	case <-time.After(jobResultTimeout):
		return nil
	}
}

func (h *Hub) registerAccept(jobID string) chan struct{} {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	ch := make(chan struct{}, 1)
	h.accepts[jobID] = ch
	return ch
}

func (h *Hub) registerResult(jobID string) chan *resultMsg {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	ch := make(chan *resultMsg, 1)
	h.results[jobID] = ch
	return ch
}

func (h *Hub) cleanupPending(jobID string) {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	delete(h.accepts, jobID)
	delete(h.results, jobID)
}

func (h *Hub) finishOrRetry(ctx context.Context, jobID string, nc *nodeConn, apiKeyID int64, code, reason string) {
	h.cleanupPending(jobID)
	var attempts int
	_ = h.pool.QueryRow(ctx,
		`UPDATE jobs SET attempts = attempts + 1, node_id = NULL WHERE id=$1 RETURNING attempts`,
		jobID).Scan(&attempts)
	if attempts < maxJobAttempts {
		log.Printf("job %s retry #%d (%s)", jobID, attempts, code)
		h.setStatus(ctx, jobID, "queued", "")
		time.AfterFunc(3*time.Second, func() { h.Enqueue(jobID) })
		return
	}
	h.setJobFailed(ctx, jobID, code, reason)
}

func (h *Hub) setStatus(ctx context.Context, jobID, status, nodeID string) {
	if nodeID == "" {
		_, _ = h.pool.Exec(ctx, `UPDATE jobs SET status=$2 WHERE id=$1`, jobID, status)
		return
	}
	_, _ = h.pool.Exec(ctx, `UPDATE jobs SET status=$2, node_id=$3 WHERE id=$1`, jobID, status, nodeID)
}

func (h *Hub) setJobDone(ctx context.Context, jobID string, cost int64) {
	_, _ = h.pool.Exec(ctx,
		`UPDATE jobs SET status='done', credits_cost=$2, finished_at=now() WHERE id=$1`, jobID, cost)
}

func (h *Hub) setJobFailed(ctx context.Context, jobID, code, reason string) {
	_, _ = h.pool.Exec(ctx,
		`UPDATE jobs SET status='failed', finished_at=now(), payload = jsonb_set(payload, '{fail}', $2::jsonb) WHERE id=$1`,
		jobID, mustJSON(map[string]string{"code": code, "reason": reason}))
}

func (h *Hub) storeArtifacts(ctx context.Context, jobID string, ras []resultArtifact) ([]storedArtifact, error) {
	arts := make([]storedArtifact, 0, len(ras))
	for i, ra := range ras {
		raw, err := base64.StdEncoding.DecodeString(ra.ContentB64)
		if err != nil || len(raw) == 0 {
			return nil, fmt.Errorf("%w: decode", errBadArtifact)
		}
		if len(raw) > artifactMaxInlineKB*1024 {
			return nil, fmt.Errorf("%w: too large (%d bytes)", errBadArtifact, len(raw))
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		if ra.SHA256 != "" && ra.SHA256 != got {
			return nil, fmt.Errorf("%w: sha mismatch", errBadArtifact)
		}
		sa := storedArtifact{Name: ra.Name, SHA256: got, Size: len(raw), Bytes: raw}
		arts = append(arts, sa)
		_, _ = h.pool.Exec(ctx,
			`INSERT INTO artifacts (job_id, storage_key, sha256, size_bytes, meta) VALUES ($1,$2,$3,$4,$5)`,
			jobID, "mem:"+jobID+":"+strconv.Itoa(i), got, len(raw), mustJSON(map[string]string{"name": ra.Name}))
	}
	if len(arts) == 0 {
		return nil, fmt.Errorf("%w: none", errBadArtifact)
	}
	h.artMu.Lock()
	h.artBank[jobID] = arts
	h.artMu.Unlock()
	return arts, nil
}

func (h *Hub) computeCost(ctx context.Context, nodeID string, runtimeS int) int64 {
	var tier int
	_ = h.pool.QueryRow(ctx, `SELECT gpu_tier FROM nodes WHERE id=$1`, nodeID).Scan(&tier)
	if tier <= 0 {
		tier = 1
	}
	if runtimeS <= 0 {
		runtimeS = 1
	}
	cost := int64(tier * runtimeS / 100)
	if cost < 1 {
		cost = 1
	}
	return cost
}

func (h *Hub) burnAndMint(ctx context.Context, apiKeyID int64, nodeID, jobID string, cost int64) bool {
	if apiKeyID == 0 {
		return true // 无需方（节点自测任务）
	}
	tag, err := h.pool.Exec(ctx,
		`UPDATE api_keys SET credits = credits - $1 WHERE id=$2 AND credits >= $1`, cost, apiKeyID)
	if err != nil || tag.RowsAffected() == 0 {
		return false
	}
	_, _ = h.pool.Exec(ctx,
		`INSERT INTO ledger (kind, api_key_id, job_id, amount, memo) VALUES ('burn',$1,$2,$3,'job')`, apiKeyID, jobID, cost)
	_, _ = h.pool.Exec(ctx,
		`INSERT INTO ledger (kind, node_id, job_id, amount, memo) VALUES ('mint',$1,$2,$3,'job')`, nodeID, jobID, cost)
	return true
}

// ---- WSS 服务端 ----

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func nodeIDFromPubkey(pk ed25519.PublicKey) string {
	sum := sha256.Sum256(pk)
	return "node_" + hex.EncodeToString(sum[:16])
}

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	// 认证：URL 带 pubkey/ts/sig，sig = ed25519.Sign("node-auth:"+ts)
	q := r.URL.Query()
	ts := q.Get("ts")
	tsi, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(tsi, 0)) > time.Minute {
		http.Error(w, "bad ts", http.StatusUnauthorized)
		return
	}
	pk, err := base64.StdEncoding.DecodeString(q.Get("pubkey"))
	if err != nil || len(pk) != ed25519.PublicKeySize {
		http.Error(w, "bad pubkey", http.StatusUnauthorized)
		return
	}
	sig, err := base64.StdEncoding.DecodeString(q.Get("sig"))
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pk), []byte("node-auth:"+ts), sig) {
		http.Error(w, "bad sig", http.StatusUnauthorized)
		return
	}
	nodeID := nodeIDFromPubkey(ed25519.PublicKey(pk))

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// 第一条消息必须是 hello
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		_ = ws.Close()
		return
	}
	var env envelope
	if json.Unmarshal(data, &env) != nil || env.Type != "hello" {
		_ = ws.Close()
		return
	}
	var hello struct {
		Pubkey string   `json:"pubkey"`
		Models []string `json:"models"`
		Tier   int      `json:"tier"`
	}
	if json.Unmarshal(env.Payload, &hello) != nil || hello.Pubkey != q.Get("pubkey") {
		_ = ws.Close()
		return
	}
	models := map[string]bool{}
	for _, m := range hello.Models {
		models[m] = true
	}

	nc := &nodeConn{id: nodeID, pubkey: ed25519.PublicKey(pk), models: models, tier: hello.Tier, ws: ws}

	h.mu.Lock()
	h.conns[nodeID] = nc
	h.mu.Unlock()

	ctx := context.Background()
	_, _ = h.pool.Exec(ctx,
		`UPDATE nodes SET status='online', last_seen=now(), models=$2::jsonb, gpu_tier=$3 WHERE id=$1`,
		nodeID, mustJSON(hello.Models), hello.Tier)
	log.Printf("node online: %s models=%v", nodeID, hello.Models)

	welcome, _ := json.Marshal(map[string]any{
		"node_id": nodeID, "heartbeat_interval_s": heartbeatIntervalS,
	})
	_ = h.send(nc, "welcome", welcome)

	defer func() {
		h.mu.Lock()
		delete(h.conns, nodeID)
		h.mu.Unlock()
		_, _ = h.pool.Exec(ctx, `UPDATE nodes SET status='offline', last_seen=now() WHERE id=$1`, nodeID)
		_ = ws.Close()
		log.Printf("node offline: %s", nodeID)
	}()

	for {
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case "heartbeat":
			// 心跳即活性；空闲遥测统计在下一迭代落库
		case "lease.accept":
			var p struct {
				JobID string `json:"job_id"`
			}
			if json.Unmarshal(env.Payload, &p) == nil {
				h.syncMu.Lock()
				if ch := h.accepts[p.JobID]; ch != nil {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
				h.syncMu.Unlock()
			}
		case "job.result", "job.fail":
			var res resultMsg
			if json.Unmarshal(env.Payload, &res) != nil {
				continue
			}
			res.OK = env.Type == "job.result"
			h.syncMu.Lock()
			if ch := h.results[res.JobID]; ch != nil {
				select {
				case ch <- &res:
				default:
				}
			}
			h.syncMu.Unlock()
		default:
			log.Printf("unknown msg type %q from %s (ignored)", env.Type, nodeID)
		}
	}
}

func (h *Hub) send(nc *nodeConn, typ string, payload json.RawMessage) error {
	env := envelope{V: protoV, Type: typ, TS: time.Now().Unix(), Payload: payload}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	nc.wmu.Lock()
	defer nc.wmu.Unlock()
	nc.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return nc.ws.WriteMessage(websocket.TextMessage, data)
}

// ---- 小工具 ----

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
