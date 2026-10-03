// Command cluster-capacity serves the cluster's remaining capacity as JSON for
// the Coder workspace creation page.
//
// The accounting mirrors the template's capacity check: usage is the sum of the
// container limits of running pods in the workspaces namespace, minus the
// reservation kept for system components. Disk usage is the sum of the PVC
// requests. Only aggregate numbers are exposed.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type gpuModel struct {
	Total    int     `json:"total"`
	MemoryGB int     `json:"memory_gb"`
	Used     float64 `json:"used"`
	Free     float64 `json:"free"`
}

type report struct {
	UpdatedAt string `json:"updated_at"`
	CPU       struct {
		Total      float64 `json:"total"`
		Overcommit float64 `json:"overcommit"`
		Budget     float64 `json:"budget"`
		Reserved   float64 `json:"reserved"`
		Used       float64 `json:"used"`
		Free       float64 `json:"free"`
	} `json:"cpu"`
	Memory struct {
		TotalGiB    float64 `json:"total_gib"`
		ReservedGiB float64 `json:"reserved_gib"`
		UsedGiB     float64 `json:"used_gib"`
		FreeGiB     float64 `json:"free_gib"`
	} `json:"memory"`
	GPU struct {
		Total  int                 `json:"total"`
		Used   float64             `json:"used"`
		Free   float64             `json:"free"`
		Models map[string]gpuModel `json:"models"`
	} `json:"gpu"`
	Disk struct {
		BudgetGB float64 `json:"budget_gb"`
		UsedGB   float64 `json:"used_gb"`
		FreeGB   float64 `json:"free_gb"`
	} `json:"disk"`
}

type config struct {
	addr          string
	namespace     string
	cpuTotal      float64
	cpuOvercommit float64
	cpuReserve    float64
	memTotal      float64
	memReserve    float64
	diskBudget    float64
	kubeconfig    string

	// HDD 持久卷
	volumeRoot         string
	volumeStorageClass string
	volumeNode         string
	// HDD 文件系统的挂载点与配额定时器写出的用量快照。
	volumeMount string
	usageFile   string
}

func kubeClient(kubeconfig string) (kubernetes.Interface, error) {
	// Prefer in-cluster credentials when running as a pod, fall back to a
	// kubeconfig file otherwise.
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	}
	path := kubeconfig
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", path, err)
	}
	return kubernetes.NewForConfig(cfg)
}

// bytesToGiB converts a storage quantity to the unit the template uses, where
// the "Gi" suffix is treated as GB (matching scripts/check-capacity.sh).
func bytesToGiB(q resource.Quantity) float64 {
	return q.AsApproximateFloat64() / (1024 * 1024 * 1024)
}

func collect(ctx context.Context, client kubernetes.Interface, cfg config) (*report, error) {
	rep := &report{UpdatedAt: time.Now().Format(time.RFC3339)}
	rep.GPU.Models = map[string]gpuModel{}

	pods, err := client.CoreV1().Pods(cfg.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, container := range pod.Spec.Containers {
			limits := container.Resources.Limits
			if q, ok := limits[corev1.ResourceCPU]; ok {
				rep.CPU.Used += q.AsApproximateFloat64()
			}
			if q, ok := limits[corev1.ResourceMemory]; ok {
				rep.Memory.UsedGiB += bytesToGiB(q)
			}
			if q, ok := limits[corev1.ResourceName("nvidia.com/gpu")]; ok {
				rep.GPU.Used += q.AsApproximateFloat64()
			}
		}
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for _, node := range nodes.Items {
		product := node.Labels["nvidia.com/gpu.product"]
		if product == "" {
			continue
		}
		total := 0
		fmt.Sscanf(node.Labels["nvidia.com/gpu.count"], "%d", &total)
		memoryMB := 0
		fmt.Sscanf(node.Labels["nvidia.com/gpu.memory"], "%d", &memoryMB)
		model := rep.GPU.Models[product]
		model.Total += total
		if gb := memoryMB / 1024; gb > model.MemoryGB {
			model.MemoryGB = gb
		}
		rep.GPU.Models[product] = model
	}
	for name, model := range rep.GPU.Models {
		rep.GPU.Total += model.Total
		// With a single model the whole usage belongs to it; with several the
		// per-model split is not knowable from limits alone.
		if len(rep.GPU.Models) == 1 {
			model.Used = rep.GPU.Used
		}
		model.Free = max(float64(model.Total)-model.Used, 0)
		rep.GPU.Models[name] = model
	}

	pvcs, err := client.CoreV1().PersistentVolumeClaims(cfg.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pvcs: %w", err)
	}
	for _, pvc := range pvcs.Items {
		// HDD 持久卷与工作区磁盘是两套账:它们有自己的容量条(卷申请页),不计入这里。
		if pvc.Labels[volumeLabel] == "true" {
			continue
		}
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			rep.Disk.UsedGB += bytesToGiB(q)
		}
	}

	// CPU is compressible, so workspaces are budgeted with an overcommit factor
	// instead of counting every requested core against the physical cores.
	rep.CPU.Total, rep.CPU.Reserved = cfg.cpuTotal, cfg.cpuReserve
	rep.CPU.Overcommit = cfg.cpuOvercommit
	rep.CPU.Budget = rep.CPU.Total * rep.CPU.Overcommit
	rep.CPU.Free = max(rep.CPU.Budget-rep.CPU.Used-rep.CPU.Reserved, 0)
	rep.Memory.TotalGiB, rep.Memory.ReservedGiB = cfg.memTotal, cfg.memReserve
	rep.Memory.FreeGiB = max(rep.Memory.TotalGiB-rep.Memory.UsedGiB-rep.Memory.ReservedGiB, 0)
	rep.GPU.Free = max(float64(rep.GPU.Total)-rep.GPU.Used, 0)
	rep.Disk.BudgetGB = cfg.diskBudget
	rep.Disk.FreeGB = max(rep.Disk.BudgetGB-rep.Disk.UsedGB, 0)
	return rep, nil
}

type server struct {
	client kubernetes.Interface
	cfg    config

	mu       sync.Mutex
	cached   *report
	cachedAt time.Time
}

func (s *server) current(ctx context.Context) (*report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && time.Since(s.cachedAt) < 3*time.Second {
		return s.cached, nil
	}
	rep, err := collect(ctx, s.client, s.cfg)
	if err != nil {
		return nil, err
	}
	s.cached, s.cachedAt = rep, time.Now()
	return rep, nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	if s.handlePreflight(w, r) {
		return
	}

	switch {
	case r.URL.Path == "/capacity":
		rep, err := s.current(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rep)
	case r.URL.Path == "/hdd":
		s.handleHDD(w, r)
	case r.URL.Path == "/volumes":
		s.handleVolumes(w, r)
	case strings.HasPrefix(r.URL.Path, "/volumes/"):
		s.handleVolumeByName(w, r)
	case r.URL.Path == "/":
		// 卷申请页:HDD 容量只在这里显示(工作区申请页不显示 HDD)
		s.handleVolumesPage(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// sweepLoop 定期清理中断的上传留下的分片,避免长期占用卷空间。
// 每个卷目录下的 .uploads 里超过保留期的分片会被删除;卷的配额脚本也会做同样的事,
// 这里保证即使定时器被停掉,服务自己也能回收临时文件。
func sweepLoop(volumeRoot string) {
	const (
		interval = time.Hour
		maxAge   = 24 * time.Hour
	)
	for {
		time.Sleep(interval)
		entries, err := os.ReadDir(volumeRoot)
		if err != nil {
			log.Printf("cluster-capacity: sweep uploads: %v", err)
			continue
		}
		total := 0
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			removed, err := sweepUploads(filepath.Join(volumeRoot, entry.Name()), maxAge)
			if err != nil {
				log.Printf("cluster-capacity: sweep uploads in %s: %v", entry.Name(), err)
				continue
			}
			total += removed
		}
		if total > 0 {
			log.Printf("cluster-capacity: swept %d stale upload chunks", total)
		}
	}
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.addr, "addr", ":3999", "address to listen on")
	flag.StringVar(&cfg.namespace, "namespace", "coder-workspaces", "namespace holding workspace pods")
	flag.Float64Var(&cfg.cpuTotal, "cpu-total", 48, "allocatable CPU cores on the node")
	flag.Float64Var(&cfg.cpuOvercommit, "cpu-overcommit", 2, "CPU overcommit factor; CPU is compressible so the budget is cores times this factor")
	flag.Float64Var(&cfg.cpuReserve, "cpu-reserved", 4, "CPU cores kept for system components")
	flag.Float64Var(&cfg.memTotal, "mem-total-gib", 377, "allocatable memory in GiB")
	flag.Float64Var(&cfg.memReserve, "mem-reserved-gib", 16, "memory in GiB kept for system components")
	flag.Float64Var(&cfg.diskBudget, "disk-budget-gb", 6800, "disk budget in GB for workspace volumes")
	flag.StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to kubeconfig (defaults to KUBECONFIG or ~/.kube/config)")
	flag.StringVar(&cfg.volumeRoot, "volume-root", "/mnt/hdd-data/volumes", "directory holding HDD volume directories")
	flag.StringVar(&cfg.volumeStorageClass, "volume-storage-class", "coder-hdd", "storage class used for HDD volume PVs")
	flag.StringVar(&cfg.volumeNode, "volume-node", "", "node the HDD volumes are pinned to (defaults to the only node in the cluster)")
	flag.StringVar(&cfg.volumeMount, "volume-mount", "/mnt/hdd-data", "mount point of the HDD filesystem holding the volume root")
	flag.StringVar(&cfg.usageFile, "usage-file", "/run/coder-hdd-usage.json", "usage snapshot written by the HDD quota timer")
	flag.Parse()

	client, err := kubeClient(cfg.kubeconfig)
	if err != nil {
		log.Fatalf("cluster-capacity: %v", err)
	}
	// 卷的 PV 用 nodeAffinity 绑到具体节点;单节点集群可以直接探测,多节点必须显式指定。
	if cfg.volumeNode == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		cancel()
		if err != nil {
			log.Fatalf("cluster-capacity: 探测节点失败,请用 -volume-node 指定: %v", err)
		}
		if len(nodes.Items) != 1 {
			log.Fatalf("cluster-capacity: 集群有 %d 个节点,请用 -volume-node 指定 HDD 卷绑定的节点", len(nodes.Items))
		}
		cfg.volumeNode = nodes.Items[0].Name
		log.Printf("cluster-capacity: HDD 卷绑定节点 %s(自动探测)", cfg.volumeNode)
	}
	srv := &server{client: client, cfg: cfg}
	go sweepLoop(cfg.volumeRoot)
	log.Printf("cluster-capacity listening on %s (namespace=%s)", cfg.addr, cfg.namespace)
	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("cluster-capacity: %v", err)
	}
}
