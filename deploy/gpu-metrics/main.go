// Command gpu-metrics 采集 GPU 历史指标并对外提供查询接口。
//
// 数据来源:NVIDIA DCGM Exporter(GPU Operator 自带,默认监听 9400)。
// 采集方式:通过 k8s API 的 Pod 代理读取 exporter 的 /metrics,不额外暴露端口。
// 存储:PostgreSQL(独立库 gpu_metrics),每分钟一张卡一行,默认保留 7 天。
// 接口:见 README;只暴露聚合后的指标,不返回容器内部信息。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type config struct {
	addr         string
	kubeconfig   string
	namespace    string
	exporterNS   string
	exporterApp  string
	interval     time.Duration
	retention    time.Duration
	pgURL        string
	exporterPort string
	coderURL     string
}

type server struct {
	cfg   config
	pool  *pgxpool.Pool
	k8s   kubernetes.Interface
	mu    sync.RWMutex
	pods  map[string]podInfo // pod 名 -> 工作区信息
	last  time.Time
	lastE error
}

type podInfo struct {
	WorkspaceID   string
	WorkspaceName string
	Username      string
}

type sample struct {
	gpuUUID        string
	hostIndex      int
	workspaceID    string
	wsName         string
	username       string
	utilPct        int
	memUsedMiB     int
	memFreeMiB     int
	memReservedMiB int
	memTotalMiB    int
	powerW         float64
	tempC          float64
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:3997", "address to serve the query API on")
	flag.StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to kubeconfig (defaults to KUBECONFIG or ~/.kube/config)")
	flag.StringVar(&cfg.namespace, "namespace", "coder-workspaces", "namespace holding workspace pods")
	flag.StringVar(&cfg.exporterNS, "exporter-namespace", "gpu-operator", "namespace of the DCGM exporter")
	flag.StringVar(&cfg.exporterApp, "exporter-label", "app=nvidia-dcgm-exporter", "label selector for the DCGM exporter pod")
	flag.StringVar(&cfg.exporterPort, "exporter-port", "9400", "DCGM exporter container port")
	flag.DurationVar(&cfg.interval, "interval", time.Minute, "sampling interval")
	flag.DurationVar(&cfg.retention, "retention", 7*24*time.Hour, "how long samples are kept")
	flag.StringVar(&cfg.pgURL, "pg-url", os.Getenv("METRICS_PG_URL"), "postgres URL (defaults to $METRICS_PG_URL)")
	flag.StringVar(&cfg.coderURL, "coder-url", "http://127.0.0.1:3001", "Coder 控制面地址,用于校验调用方会话")
	flag.Parse()

	if cfg.pgURL == "" {
		log.Fatal("gpu-metrics: 需要 -pg-url 或环境变量 METRICS_PG_URL")
	}

	restCfg, err := kubeClientConfig(cfg.kubeconfig)
	if err != nil {
		log.Fatalf("gpu-metrics: %v", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Fatalf("gpu-metrics: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.pgURL)
	if err != nil {
		log.Fatalf("gpu-metrics: 连接数据库失败: %v", err)
	}
	defer pool.Close()
	if err := migrate(ctx, pool); err != nil {
		log.Fatalf("gpu-metrics: 初始化表失败: %v", err)
	}

	srv := &server{cfg: cfg, pool: pool, k8s: client, pods: map[string]podInfo{}}
	go srv.collectLoop()
	go srv.cleanupLoop()

	log.Printf("gpu-metrics listening on %s (interval=%s retention=%s)", cfg.addr, cfg.interval, cfg.retention)
	httpSrv := &http.Server{Addr: cfg.addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("gpu-metrics: %v", err)
	}
}

func kubeClientConfig(path string) (*rest.Config, error) {
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".kube", "config")
	}
	return clientcmd.BuildConfigFromFlags("", path)
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		create table if not exists gpu_samples (
			sampled_at     timestamptz not null,
			gpu_uuid       text        not null,
			host_index     int         not null,
			workspace_id   text        not null,
			workspace_name text        not null,
			username       text        not null,
			util_pct       smallint    not null,
			mem_used_mib   int         not null,
			mem_total_mib  int         not null,
			power_w        real,
			temp_c         real,
			primary key (gpu_uuid, sampled_at)
		);
		create index if not exists gpu_samples_ws_time on gpu_samples (workspace_id, sampled_at desc);
	`)
	return err
}

// ---------- 采集 ----------

func (s *server) collectLoop() {
	s.collectOnce()
	ticker := time.NewTicker(s.cfg.interval)
	defer ticker.Stop()
	for range ticker.C {
		s.collectOnce()
	}
}

func (s *server) collectOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := s.scrapeExporter(ctx)
	if err != nil {
		s.setError(err)
		log.Printf("gpu-metrics: 采集失败: %v", err)
		return
	}
	samples, err := s.parseSamples(ctx, raw)
	if err != nil {
		s.setError(err)
		log.Printf("gpu-metrics: 解析失败: %v", err)
		return
	}
	if len(samples) == 0 {
		log.Printf("gpu-metrics: 本次没有可记录的样本(exporter 输出 %d 字节)", len(raw))
		s.setOK()
		return
	}
	ts := time.Now().Truncate(time.Minute)
	for _, sm := range samples {
		_, err := s.pool.Exec(ctx, `
			insert into gpu_samples (sampled_at, gpu_uuid, host_index, workspace_id, workspace_name,
				username, util_pct, mem_used_mib, mem_total_mib, power_w, temp_c)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			on conflict (gpu_uuid, sampled_at) do update set
				util_pct = excluded.util_pct, mem_used_mib = excluded.mem_used_mib,
				mem_total_mib = excluded.mem_total_mib, power_w = excluded.power_w,
				temp_c = excluded.temp_c, workspace_name = excluded.workspace_name,
				username = excluded.username
		`, ts, sm.gpuUUID, sm.hostIndex, sm.workspaceID, sm.wsName, sm.username,
			sm.utilPct, sm.memUsedMiB, sm.memTotalMiB, sm.powerW, sm.tempC)
		if err != nil {
			log.Printf("gpu-metrics: 写库失败: %v", err)
			return
		}
	}
	s.setOK()
	log.Printf("gpu-metrics: 已记录 %d 张卡的样本", len(samples))
}

// scrapeExporter 通过 k8s API 的 Pod 代理读取 DCGM exporter 的指标。
func (s *server) scrapeExporter(ctx context.Context) (string, error) {
	pods, err := s.k8s.CoreV1().Pods(s.cfg.exporterNS).List(ctx, metav1.ListOptions{LabelSelector: s.cfg.exporterApp})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("在 %s 里找不到 DCGM exporter(%s)", s.cfg.exporterNS, s.cfg.exporterApp)
	}
	pod := pods.Items[0].Name
	body, err := s.k8s.CoreV1().Pods(s.cfg.exporterNS).ProxyGet("http", pod, s.cfg.exporterPort, "/metrics", nil).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// parseSamples 解析 Prometheus 文本格式,按 (pod, gpu) 归并需要的指标。
func (s *server) parseSamples(ctx context.Context, raw string) ([]sample, error) {
	type key struct{ pod, gpu string }
	acc := map[key]*sample{}
	get := func(pod, container, gpu, uuid string) *sample {
		k := key{pod: pod, gpu: gpu}
		sm, ok := acc[k]
		if !ok {
			info, err := s.podInfo(ctx, pod)
			if err != nil || info.WorkspaceID == "" {
				return nil
			}
			sm = &sample{gpuUUID: uuid, workspaceID: info.WorkspaceID, wsName: info.WorkspaceName, username: info.Username}
			acc[k] = sm
		}
		return sm
	}

	for _, line := range strings.Split(raw, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parseLine(line)
		if !ok {
			continue
		}
		switch name {
		case "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_FB_FREE",
			"DCGM_FI_DEV_FB_RESERVED", "DCGM_FI_DEV_FB_TOTAL",
			"DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_GPU_TEMP":
		default:
			continue
		}
		pod := labels["pod"]
		if pod == "" {
			continue // 未被容器占用的卡
		}
		hostIdx, _ := strconv.Atoi(labels["gpu"])
		sm := get(pod, labels["container"], labels["gpu"], labels["UUID"])
		if sm == nil {
			continue
		}
		sm.hostIndex = hostIdx
		switch name {
		case "DCGM_FI_DEV_GPU_UTIL":
			sm.utilPct = int(value)
		case "DCGM_FI_DEV_FB_USED":
			sm.memUsedMiB = int(value)
		case "DCGM_FI_DEV_FB_FREE":
			sm.memFreeMiB = int(value)
		case "DCGM_FI_DEV_FB_RESERVED":
			sm.memReservedMiB = int(value)
		case "DCGM_FI_DEV_FB_TOTAL":
			sm.memTotalMiB = int(value)
		case "DCGM_FI_DEV_POWER_USAGE":
			sm.powerW = value
		case "DCGM_FI_DEV_GPU_TEMP":
			sm.tempC = value
		}
	}

	out := make([]sample, 0, len(acc))
	for _, sm := range acc {
		// DCGM 只报 FREE/USED/RESERVED,没有 TOTAL;三者相加即为该卡显存总量。
		if sm.memTotalMiB == 0 {
			sm.memTotalMiB = sm.memUsedMiB + sm.memFreeMiB + sm.memReservedMiB
		}
		if sm.memTotalMiB == 0 {
			continue
		}
		out = append(out, *sm)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].workspaceID != out[j].workspaceID {
			return out[i].workspaceID < out[j].workspaceID
		}
		return out[i].hostIndex < out[j].hostIndex
	})
	return out, nil
}

// parseLine 解析一行 Prometheus 指标:name{k="v",...} value
func parseLine(line string) (string, map[string]string, float64, bool) {
	brace := strings.IndexByte(line, '{')
	var name, labelStr, valStr string
	if brace < 0 {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return "", nil, 0, false
		}
		name, valStr = fields[0], fields[1]
	} else {
		end := strings.LastIndexByte(line, '}')
		if end < brace {
			return "", nil, 0, false
		}
		name = line[:brace]
		labelStr = line[brace+1 : end]
		valStr = strings.TrimSpace(line[end+1:])
	}
	value, err := strconv.ParseFloat(strings.Fields(valStr)[0], 64)
	if err != nil {
		return "", nil, 0, false
	}
	labels := map[string]string{}
	for _, pair := range splitLabels(labelStr) {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		labels[strings.TrimSpace(kv[0])] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
	}
	return name, labels, value, true
}

// splitLabels 按逗号切分标签,忽略引号内的逗号。
func splitLabels(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// podInfo 从 Pod 标签解析工作区信息,结果缓存。
func (s *server) podInfo(ctx context.Context, pod string) (podInfo, error) {
	s.mu.RLock()
	info, ok := s.pods[pod]
	s.mu.RUnlock()
	if ok {
		return info, nil
	}
	p, err := s.k8s.CoreV1().Pods(s.cfg.namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return podInfo{}, err
	}
	info = podInfo{
		WorkspaceID:   p.Labels["com.coder.workspace.id"],
		WorkspaceName: p.Labels["com.coder.workspace.name"],
		Username:      p.Labels["com.coder.user.username"],
	}
	if info.WorkspaceID != "" {
		s.mu.Lock()
		s.pods[pod] = info
		s.mu.Unlock()
	}
	return info, nil
}

func (s *server) setOK() {
	s.mu.Lock()
	s.last, s.lastE = time.Now(), nil
	s.mu.Unlock()
}

func (s *server) setError(err error) {
	s.mu.Lock()
	s.last, s.lastE = time.Now(), err
	s.mu.Unlock()
}

func (s *server) cleanupLoop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		tag, err := s.pool.Exec(ctx, "delete from gpu_samples where sampled_at < now() - make_interval(hours => $1)", int(s.cfg.retention.Hours()))
		cancel()
		if err != nil {
			log.Printf("gpu-metrics: 清理失败: %v", err)
			continue
		}
		if tag.RowsAffected() > 0 {
			log.Printf("gpu-metrics: 清理了 %d 条超过保留期的样本", tag.RowsAffected())
		}
	}
}

// ---------- 查询接口 ----------

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case r.URL.Path == "/healthz":
		s.handleHealthz(w)
	case r.URL.Path == "/gpus":
		s.handleGPUs(w, r)
	case r.URL.Path == "/series":
		s.handleSeries(w, r)
	case r.URL.Path == "/dashboard":
		s.handleDashboard(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleHealthz(w http.ResponseWriter) {
	s.mu.RLock()
	last, lastErr := s.last, s.lastE
	s.mu.RUnlock()
	status := "ok"
	if lastErr != nil {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": status, "last_sample_at": last, "last_error": errString(lastErr),
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type gpuRow struct {
	UUID        string `json:"uuid"`
	HostIndex   int    `json:"host_index"`
	Index       int    `json:"index"`
	MemTotalMiB int    `json:"mem_total_mib"`
	LastSeen    string `json:"last_seen"`
}

// handleGPUs 列出某个工作区最近出现过的 GPU。
func (s *server) handleGPUs(w http.ResponseWriter, r *http.Request) {
	wsID := r.URL.Query().Get("workspace_id")
	if wsID == "" {
		writeError(w, http.StatusBadRequest, "缺少 workspace_id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		select gpu_uuid, host_index, max(mem_total_mib), max(sampled_at)
		from gpu_samples
		where workspace_id = $1 and sampled_at > now() - interval '7 days'
		group by gpu_uuid, host_index
		order by host_index`, wsID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	var out []gpuRow
	for rows.Next() {
		var g gpuRow
		var last time.Time
		if err := rows.Scan(&g.UUID, &g.HostIndex, &g.MemTotalMiB, &last); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		g.LastSeen = last.UTC().Format(time.RFC3339)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HostIndex < out[j].HostIndex })
	for i := range out {
		out[i].Index = i // 工作区内的序号,从 0 开始
	}
	if out == nil {
		out = []gpuRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"gpus": out})
}

type point struct {
	T          string  `json:"t"`
	UtilPct    float64 `json:"util_pct"`
	MemUsedMiB float64 `json:"mem_used_mib"`
	MemTotal   int     `json:"mem_total_mib"`
	PowerW     float64 `json:"power_w"`
	TempC      float64 `json:"temp_c"`
}

// handleSeries 返回某个工作区某张卡的历史序列,自动降采样到 600 点以内。
func (s *server) handleSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	wsID, gpu := q.Get("workspace_id"), q.Get("gpu")
	if wsID == "" || gpu == "" {
		writeError(w, http.StatusBadRequest, "缺少 workspace_id 或 gpu")
		return
	}
	hours, err := strconv.Atoi(q.Get("hours"))
	if err != nil || hours <= 0 {
		hours = 8
	}
	if hours > 24*7 {
		hours = 24 * 7
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var total int
	if err := s.pool.QueryRow(ctx, `select count(*) from gpu_samples where workspace_id=$1 and gpu_uuid=$2 and sampled_at > now() - make_interval(hours => $3)`,
		wsID, gpu, hours).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	bucket := 1
	if total > 600 {
		bucket = (total + 599) / 600
	}

	rows, err := s.pool.Query(ctx, `
		select to_timestamp(floor(extract(epoch from sampled_at) / ($4 * 60)) * ($4 * 60)) as bucket,
		       avg(util_pct), avg(mem_used_mib), max(mem_total_mib), avg(power_w), avg(temp_c)
		from gpu_samples
		where workspace_id=$1 and gpu_uuid=$2 and sampled_at > now() - make_interval(hours => $3)
		group by bucket
		order by bucket`, wsID, gpu, hours, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	points := []point{}
	for rows.Next() {
		var p point
		var ts time.Time
		if err := rows.Scan(&ts, &p.UtilPct, &p.MemUsedMiB, &p.MemTotal, &p.PowerW, &p.TempC); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p.T = ts.UTC().Format(time.RFC3339)
		points = append(points, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace_id": wsID, "gpu": gpu, "hours": hours,
		"bucket_minutes": bucket, "total_raw_points": total, "points": points,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---------- 只读工作区看板 ----------

type gpuUsage struct {
	Index       int     `json:"index"`
	HostIndex   int     `json:"host_index"`
	UtilPct     float64 `json:"util_pct"`
	MemUsedMiB  float64 `json:"mem_used_mib"`
	MemTotalMiB float64 `json:"mem_total_mib"`
}

type instance struct {
	WorkspaceID   string     `json:"workspace_id"`
	WorkspaceName string     `json:"workspace_name"`
	Username      string     `json:"username"`
	Status        string     `json:"status"`
	StartedAt     string     `json:"started_at"`
	UptimeSeconds int64      `json:"uptime_seconds"`
	CPULimit      float64    `json:"cpu_limit"`
	MemoryGiB     float64    `json:"memory_gib"`
	GPUCount      int        `json:"gpu_count"`
	DiskGiB       int        `json:"disk_gib"`
	UsedCPUCores  float64    `json:"used_cpu_cores"`
	UsedMemoryGiB float64    `json:"used_memory_gib"`
	DiskUsedGiB   float64    `json:"disk_used_gib"`
	GPUs          []gpuUsage `json:"gpus"`
}

// requireUser 用调用方自己的会话向控制面确认身份:只允许已登录用户读取。
func (s *server) requireUser(w http.ResponseWriter, r *http.Request) bool {
	token := r.Header.Get("Coder-Session-Token")
	if token == "" {
		if c, err := r.Cookie("coder_session_token"); err == nil {
			token = c.Value
		}
	}
	if token == "" {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.coderURL+"/api/v2/users/me", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	req.Header.Set("Coder-Session-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return false
	}
	return true
}

// handleDashboard 列出所有正在运行的工作区,附带配置与当前用量。只读,不提供任何操作入口。
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	pods, err := s.k8s.CoreV1().Pods(s.cfg.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 每个工作区最近一次 GPU 采样
	gpusByWS := map[string][]gpuUsage{}
	rows, err := s.pool.Query(ctx, `
		with latest as (
			select workspace_id, max(sampled_at) as ts
			from gpu_samples
			where sampled_at > now() - interval '10 minutes'
			group by workspace_id
		)
		select g.workspace_id, g.host_index, g.util_pct, g.mem_used_mib, g.mem_total_mib
		from gpu_samples g
		join latest l on l.workspace_id = g.workspace_id and l.ts = g.sampled_at
		order by g.workspace_id, g.host_index`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	for rows.Next() {
		var wsID string
		var g gpuUsage
		if err := rows.Scan(&wsID, &g.HostIndex, &g.UtilPct, &g.MemUsedMiB, &g.MemTotalMiB); err != nil {
			continue
		}
		gpusByWS[wsID] = append(gpusByWS[wsID], g)
	}

	// kubelet Summary API:CPU / 内存 / 卷用量的当前值(不依赖 metrics-server)
	usage := s.nodeUsage(ctx, pods)

	instances := []instance{}
	now := time.Now()
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		wsID := p.Labels["com.coder.workspace.id"]
		if wsID == "" {
			continue
		}
		inst := instance{
			WorkspaceID:   wsID,
			WorkspaceName: p.Labels["com.coder.workspace.name"],
			Username:      p.Labels["com.coder.user.username"],
			Status:        string(p.Status.Phase),
			StartedAt:     p.CreationTimestamp.UTC().Format(time.RFC3339),
			UptimeSeconds: int64(now.Sub(p.CreationTimestamp.Time).Seconds()),
		}
		if p.Status.StartTime != nil {
			inst.StartedAt = p.Status.StartTime.UTC().Format(time.RFC3339)
			inst.UptimeSeconds = int64(now.Sub(p.Status.StartTime.Time).Seconds())
		}
		for _, c := range p.Spec.Containers {
			if c.Name != "dev" {
				continue
			}
			inst.CPULimit = parseCPU(c.Resources.Limits.Cpu().String())
			inst.MemoryGiB = float64(c.Resources.Limits.Memory().Value()) / (1 << 30)
			if g, ok := c.Resources.Limits["nvidia.com/gpu"]; ok {
				inst.GPUCount = int(g.Value())
			}
		}
		if u, ok := usage[p.Name]; ok {
			inst.UsedCPUCores = u.cpuCores
			inst.UsedMemoryGiB = u.memoryGiB
		}
		homeClaim, diskGiB := s.homeClaim(ctx, &p)
		inst.DiskGiB = diskGiB
		if u, ok := usage[p.Name]; ok {
			inst.DiskUsedGiB = u.diskByClaim[homeClaim]
		}
		for i := range gpusByWS[wsID] {
			gpusByWS[wsID][i].Index = i
		}
		inst.GPUs = gpusByWS[wsID]
		if inst.GPUs == nil {
			inst.GPUs = []gpuUsage{}
		}
		instances = append(instances, inst)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Username < instances[j].Username })
	writeJSON(w, http.StatusOK, map[string]any{"instances": instances})
}

type podUsage struct {
	cpuCores    float64
	memoryGiB   float64
	diskByClaim map[string]float64
}

// nodeUsage 读取 kubelet Summary API(经 k8s API 的节点代理),得到每个 Pod 的当前用量。
func (s *server) nodeUsage(ctx context.Context, pods *corev1.PodList) map[string]podUsage {
	out := map[string]podUsage{}
	nodes := map[string]bool{}
	for _, p := range pods.Items {
		if p.Spec.NodeName != "" {
			nodes[p.Spec.NodeName] = true
		}
	}
	for node := range nodes {
		raw, err := s.k8s.CoreV1().RESTClient().Get().
			Resource("nodes").Name(node).SubResource("proxy").Suffix("stats/summary").DoRaw(ctx)
		if err != nil {
			log.Printf("gpu-metrics: 读取 kubelet summary 失败(%s): %v", node, err)
			continue
		}
		var summary struct {
			Pods []struct {
				PodRef struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"podRef"`
				CPU struct {
					UsageNanoCores int64 `json:"usageNanoCores"`
				} `json:"cpu"`
				Memory struct {
					WorkingSetBytes int64 `json:"workingSetBytes"`
				} `json:"memory"`
				Containers []struct {
					CPU struct {
						UsageNanoCores int64 `json:"usageNanoCores"`
					} `json:"cpu"`
					Memory struct {
						WorkingSetBytes int64 `json:"workingSetBytes"`
					} `json:"memory"`
				} `json:"containers"`
				Volume []struct {
					UsedBytes *int64 `json:"usedBytes"`
					PVCRef    *struct {
						Name string `json:"name"`
					} `json:"pvcRef"`
				} `json:"volume"`
			} `json:"pods"`
		}
		if err := json.Unmarshal(raw, &summary); err != nil {
			log.Printf("gpu-metrics: 解析 kubelet summary 失败: %v", err)
			continue
		}
		for _, p := range summary.Pods {
			if p.PodRef.Namespace != s.cfg.namespace {
				continue
			}
			u := podUsage{cpuCores: float64(p.CPU.UsageNanoCores) / 1e9, memoryGiB: float64(p.Memory.WorkingSetBytes) / (1 << 30), diskByClaim: map[string]float64{}}
			if u.cpuCores == 0 && u.memoryGiB == 0 {
				for _, c := range p.Containers {
					u.cpuCores += float64(c.CPU.UsageNanoCores) / 1e9
					u.memoryGiB += float64(c.Memory.WorkingSetBytes) / (1 << 30)
				}
			}
			u.diskByClaim = map[string]float64{}
			for _, v := range p.Volume {
				if v.UsedBytes == nil || v.PVCRef == nil {
					continue
				}
				u.diskByClaim[v.PVCRef.Name] = float64(*v.UsedBytes) / (1 << 30)
			}
			out[p.PodRef.Name] = u
		}
	}
	return out
}

// homeClaim 找出挂载到 /home/coder 的 PVC 名与容量(GiB)。工作区还挂着 HDD 卷,
// 不能随便取第一个 PVC。
func (s *server) homeClaim(ctx context.Context, p *corev1.Pod) (string, int) {
	name := ""
	for _, c := range p.Spec.Containers {
		if c.Name != "dev" {
			continue
		}
		for _, m := range c.VolumeMounts {
			if m.MountPath == "/home/coder" {
				name = m.Name
			}
		}
	}
	if name == "" {
		return "", 0
	}
	for _, v := range p.Spec.Volumes {
		if v.Name != name || v.PersistentVolumeClaim == nil {
			continue
		}
		claim := v.PersistentVolumeClaim.ClaimName
		pvc, err := s.k8s.CoreV1().PersistentVolumeClaims(s.cfg.namespace).Get(ctx, claim, metav1.GetOptions{})
		if err != nil {
			return claim, 0
		}
		if size, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			return claim, int(size.Value() / (1 << 30))
		}
		return claim, 0
	}
	return "", 0
}

func parseCPU(s string) float64 {
	if strings.HasSuffix(s, "m") {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
		return v / 1000
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
