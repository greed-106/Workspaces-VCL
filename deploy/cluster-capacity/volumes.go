package main

// HDD 持久卷:用户申请一块 HDD 空间,冷数据放里面,可以反复挂到自己的任意工作区。
// 卷在工作区之外创建(本服务直接建 目录 + PV + PVC),因此工作区删/建都不影响数据。
//
// 约定:
//   目录     /mnt/hdd-data/volumes/<owner>-<name>
//   PVC/PV   hdd-<owner>-<name>(PVC 在 coder-workspaces 命名空间)
//   配额     由 coder-hdd-volume-quota.py 按 PVC 申请值设置(HDD 需以 prjquota 挂载)
//
// 容量限制只在前端页面做(单卷 100-1000G,且是每人的总量上限);API 不限制。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	volumeLabel      = "coder-hdd-volume"
	volumeOwnerLabel = "coder-hdd-owner"
	volumeNameLabel  = "coder-hdd-name"
	volumePathAnno   = "coder.com/hdd-volume-path"
	volumeSizeAnno   = "coder.com/hdd-volume-size-gb"
)

type volume struct {
	Name   string  `json:"name"`
	Owner  string  `json:"owner"`
	PVC    string  `json:"pvc"`
	Path   string  `json:"path"`
	SizeGB float64 `json:"size_gb"`
	// UsedGB 是该卷目录的实际占用(null = 快照不可用),LimitGB 是 XFS project
	// quota 上的硬上限(目前由 PVC 的申请容量驱动,后续扩容/缩容时它就是生效值)。
	UsedGB  *float64 `json:"used_gb"`
	LimitGB float64  `json:"limit_gb"`
	Phase   string   `json:"phase"`
	InUseBy []string `json:"in_use_by"`
}

type hddInfo struct {
	TotalGB float64 `json:"total_gb"`
	FreeGB  float64 `json:"free_gb"`
	UsedGB  float64 `json:"used_gb"`
	Volumes int     `json:"volumes"`
	Quota   bool    `json:"prjquota_active"`
}

// usageSnapshot 读取配额定时器(root)写出的用量快照:/run/coder-hdd-usage.json。
// 服务以普通用户运行,读不了 xfs_quota 的 project 报表,因此由定时器代劳,
// 文件缺失或过期时返回 ok=false,页面显示「—」而不是假装 0。
func (s *server) usageSnapshot() (map[string][2]float64, bool) {
	raw, err := os.ReadFile(s.cfg.usageFile)
	if err != nil {
		return nil, false
	}
	var snap struct {
		UpdatedAt int64 `json:"updated_at"`
		Volumes   map[string]struct {
			UsedBytes  float64 `json:"used_bytes"`
			LimitBytes float64 `json:"limit_bytes"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, false
	}
	out := map[string][2]float64{}
	for name, v := range snap.Volumes {
		out[name] = [2]float64{v.UsedBytes, v.LimitBytes}
	}
	return out, true
}

// names 返回 (PVC 名, 目录名)
func (cfg config) volumeNames(owner, name string) (string, string) {
	full := owner + "-" + name
	return "hdd-" + full, full
}

func (s *server) volumeDir(owner, name string) string {
	_, dir := s.cfg.volumeNames(owner, name)
	return filepath.Join(s.cfg.volumeRoot, dir)
}

// inUse 返回正在运行、且挂载了该 PVC 的 Pod 名(单节点上 k8s 不会阻止双挂,必须自己查)
func (s *server) inUse(ctx context.Context) (map[string][]string, error) {
	pods, err := s.client.CoreV1().Pods(s.cfg.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	used := map[string][]string{}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim == nil {
				continue
			}
			claim := v.PersistentVolumeClaim.ClaimName
			used[claim] = append(used[claim], pod.Name)
		}
	}
	return used, nil
}

func (s *server) listVolumes(ctx context.Context) ([]volume, error) {
	pvcs, err := s.client.CoreV1().PersistentVolumeClaims(s.cfg.namespace).List(ctx,
		metav1.ListOptions{LabelSelector: volumeLabel + "=true"})
	if err != nil {
		return nil, fmt.Errorf("list pvcs: %w", err)
	}
	used, err := s.inUse(ctx)
	if err != nil {
		return nil, err
	}
	// 用量快照拿不到时不影响列表,只是不显示实际用量。
	quota, quotaOK := s.usageSnapshot()
	out := []volume{}
	for _, pvc := range pvcs.Items {
		owner := pvc.Labels[volumeOwnerLabel]
		name := pvc.Labels[volumeNameLabel]
		if name == "" { // 兼容手工创建的卷:从 hdd-<owner>-<name> 推导
			name = strings.TrimPrefix(strings.TrimPrefix(pvc.Name, "hdd-"), owner+"-")
		}
		if owner == "" {
			owner = strings.SplitN(strings.TrimPrefix(pvc.Name, "hdd-"), "-", 2)[0]
		}
		path := pvc.Annotations[volumePathAnno]
		if path == "" {
			path = filepath.Join(s.cfg.volumeRoot, strings.TrimPrefix(pvc.Name, "hdd-"))
		}
		sizeGB := 0.0
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			sizeGB = bytesToGiB(q)
		}
		limitGB := sizeGB
		var usedGB *float64
		if q, ok := quota[pvc.Name]; ok && quotaOK {
			v := q[0] / (1 << 30)
			usedGB = &v
			if q[1] > 0 {
				limitGB = q[1] / (1 << 30)
			}
		}
		out = append(out, volume{
			Name: name, Owner: owner, PVC: pvc.Name, Path: path,
			SizeGB: sizeGB, UsedGB: usedGB, LimitGB: limitGB,
			Phase: string(pvc.Status.Phase), InUseBy: used[pvc.Name],
		})
	}
	return out, nil
}

func (s *server) getVolume(ctx context.Context, pvcName string) (*volume, error) {
	all, err := s.listVolumes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].PVC == pvcName {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("卷 %s 不存在", pvcName)
}

func (s *server) createVolume(ctx context.Context, owner, name string, sizeGB int) (*volume, error) {
	if owner == "" || name == "" {
		return nil, errors.New("owner 与 name 都不能为空")
	}
	if strings.ContainsAny(owner+name, "/ \t\n") {
		return nil, errors.New("名字里不能有空格或斜杠")
	}
	pvcName, dirName := s.cfg.volumeNames(owner, name)
	dir := filepath.Join(s.cfg.volumeRoot, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("建目录失败: %w", err)
	}
	qty := resource.MustParse(strconv.Itoa(sizeGB) + "Gi")
	labels := map[string]string{
		volumeLabel: "true", volumeOwnerLabel: owner, volumeNameLabel: name,
	}
	annos := map[string]string{
		volumePathAnno: dir,
		volumeSizeAnno: strconv.Itoa(sizeGB),
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Labels: labels, Annotations: annos},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: qty},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              s.cfg.volumeStorageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				Local: &corev1.LocalVolumeSource{Path: dir},
			},
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "kubernetes.io/hostname",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{s.cfg.volumeNode},
					}},
				}}},
			},
		},
	}
	if _, err := s.client.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("建 PV 失败: %w", err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName, Namespace: s.cfg.namespace, Labels: labels, Annotations: annos,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &s.cfg.volumeStorageClass,
			VolumeName:       pvcName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	if _, err := s.client.CoreV1().PersistentVolumeClaims(s.cfg.namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		_ = s.client.CoreV1().PersistentVolumes().Delete(ctx, pvcName, metav1.DeleteOptions{})
		return nil, fmt.Errorf("建 PVC 失败: %w", err)
	}
	return s.getVolume(ctx, pvcName)
}

// deleteVolume 删掉 k8s 对象;purge=true 时连数据目录一起删。
func (s *server) deleteVolume(ctx context.Context, pvcName string, purge bool) error {
	vol, err := s.getVolume(ctx, pvcName)
	if err != nil {
		return err
	}
	if len(vol.InUseBy) > 0 {
		return fmt.Errorf("卷正被工作区使用中(%s),请先停止该工作区", strings.Join(vol.InUseBy, ", "))
	}
	_ = cleanupUploads(vol.Path) // 卷里的临时分片一并清掉
	_ = s.client.CoreV1().PersistentVolumeClaims(s.cfg.namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
	if err := s.client.CoreV1().PersistentVolumes().Delete(ctx, pvcName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("删 PV 失败: %w", err)
	}
	if purge {
		if err := os.RemoveAll(vol.Path); err != nil {
			return fmt.Errorf("删数据目录失败: %w", err)
		}
	}
	return nil
}

func (s *server) hddCapacity(ctx context.Context) (hddInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.volumeRoot, &st); err != nil {
		return hddInfo{}, err
	}
	total := float64(st.Blocks) * float64(st.Bsize) / (1024 * 1024 * 1024)
	free := float64(st.Bavail) * float64(st.Bsize) / (1024 * 1024 * 1024)
	vols, err := s.listVolumes(ctx)
	if err != nil {
		return hddInfo{}, err
	}
	opts, _ := os.ReadFile("/proc/mounts")
	quota := false
	for _, line := range strings.Split(string(opts), "\n") {
		if strings.HasPrefix(line, "/dev/mapper/hdd--vg-hdd--data") && strings.Contains(line, "prjquota") {
			quota = true
		}
	}
	return hddInfo{
		TotalGB: total, FreeGB: free, UsedGB: total - free,
		Volumes: len(vols), Quota: quota,
	}, nil
}

// ---------- HTTP ----------

// handlePreflight 处理跨域预检:前端在 :3001(SPA)而本服务在 :3999,POST/DELETE + JSON
// 都会先发 OPTIONS,不回应的话请求会一直挂着。
func (s *server) handlePreflight(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		vols, err := s.listVolumes(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"volumes": vols})
	case http.MethodPost:
		var req struct {
			Owner  string `json:"owner"`
			Name   string `json:"name"`
			SizeGB int    `json:"size_gb"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
			return
		}
		if req.SizeGB <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "size_gb 必须大于 0"})
			return
		}
		vol, err := s.createVolume(r.Context(), req.Owner, req.Name, req.SizeGB)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, vol)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *server) handleVolumeByName(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/uploads") {
		s.handleUpload(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/volumes/")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少卷名"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		vol, err := s.getVolume(r.Context(), name)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, vol)
	case http.MethodDelete:
		purge := r.URL.Query().Get("purge") == "true"
		var req struct {
			Confirm string `json:"confirm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Confirm != name {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "删除需要二次确认:请求体传 {\"confirm\":\"" + name + "\"}"})
			return
		}
		if err := s.deleteVolume(r.Context(), name, purge); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": name, "purged": strconv.FormatBool(purge)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *server) handleHDD(w http.ResponseWriter, r *http.Request) {
	info, err := s.hddCapacity(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) handleVolumesPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(volumesPageHTML))
}
