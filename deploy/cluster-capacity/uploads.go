package main

// 数据卷的文件上传:分片 + 断点续传 + 原子落盘。
//
// 设计要点(尽量少的状态):
//   上传状态 = 卷里 .uploads/ 下的一个分片文件,名字里带上传 id 与目标文件名,
//   因此服务重启也不丢进度,不需要数据库或会话表。
//   分片以偏移量追加,PATCH 自带幂等性;完成时校验大小并 rename 到卷根目录(原子)。
//   临时文件不会出现在卷根目录,用户看不到半截文件。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const uploadDirName = ".uploads"
const maxUploadChunk = 64 << 20 // 单片上限 64MiB

// safeName 只保留文件名本身,拒绝路径穿越与隐藏文件。
func safeName(name string) (string, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return "", errors.New("文件名不合法")
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return "", errors.New("文件名不能包含斜杠")
	}
	return name, nil
}

// volumeRoot 返回卷在宿主上的目录(借 listVolumes 找到 path)。
func (s *server) volumeRoot(ctx context.Context, pvc string) (string, error) {
	vol, err := s.getVolume(ctx, pvc)
	if err != nil {
		return "", err
	}
	return vol.Path, nil
}

func (s *server) uploadsDir(root string) string { return filepath.Join(root, uploadDirName) }

// findPart 用 id 在 .uploads/ 下找分片文件(名字格式 <id>.<filename>.part)。
func (s *server) findPart(root, id string) (string, string, error) {
	matches, err := filepath.Glob(filepath.Join(s.uploadsDir(root), id+".*.part"))
	if err != nil || len(matches) == 0 {
		return "", "", errors.New("上传会话不存在或已过期")
	}
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(matches[0]), id+"."), ".part")
	return matches[0], name, nil
}

// freeName 在目标目录里找一个不冲突的名字:dataset.zip → dataset(1).zip
func freeName(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s(%d)%s", base, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s.%d%s", base, os.Getpid(), ext))
}

// handleUpload 负责 /volumes/{pvc}/uploads[/{id}[/complete]]
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/volumes/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "uploads" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	root, err := s.volumeRoot(r.Context(), parts[0])
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(s.uploadsDir(root), 0o770); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	switch {
	case len(parts) == 2 && r.Method == http.MethodPost:
		s.startUpload(w, r, root)
	case len(parts) == 3 && (r.Method == http.MethodHead || r.Method == http.MethodGet):
		s.uploadStatus(w, r, root, parts[2])
	case len(parts) == 3 && r.Method == http.MethodPatch:
		s.appendUpload(w, r, root, parts[2])
	case len(parts) == 3 && r.Method == http.MethodDelete:
		s.cancelUpload(w, r, root, parts[2])
	case len(parts) == 4 && parts[3] == "complete" && r.Method == http.MethodPost:
		s.completeUpload(w, r, root, parts[2])
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// freeBytes 用 statfs 读卷的可用空间。卷开了 XFS project quota 后,
// statfs 报的就是这个卷的剩余额度(而不是整块盘),正是我们要的口径。
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (s *server) startUpload(w http.ResponseWriter, r *http.Request, root string) {
	name, err := safeName(r.URL.Query().Get("filename"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	part := filepath.Join(s.uploadsDir(root), id+"."+name+".part")
	file, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o660)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = file.Close()
	_ = os.Chmod(part, 0o660)
	// 预检:声明的大小超过卷的剩余额度就直接拒绝,不让用户白传几个 GB。
	if want := r.URL.Query().Get("size"); want != "" {
		size, parseErr := strconv.ParseInt(want, 10, 64)
		if parseErr != nil || size < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "size 不合法"})
			return
		}
		if free, freeErr := freeBytes(root); freeErr == nil && size > free {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": fmt.Sprintf("文件太大:需要 %.1f GB,卷内仅剩 %.1f GB,请先清理或申请更大的卷",
					float64(size)/float64(1<<30), float64(free)/float64(1<<30)),
				"free_bytes": free})
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"upload_id": id, "filename": name, "offset": 0})
}

func (s *server) uploadStatus(w http.ResponseWriter, r *http.Request, root, id string) {
	part, name, err := s.findPart(root, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	info, err := os.Stat(part)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("X-Upload-Offset", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Upload-Filename", name)
	w.Header().Set("Access-Control-Expose-Headers", "X-Upload-Offset, X-Upload-Filename")
	writeJSON(w, http.StatusOK, map[string]any{"upload_id": id, "filename": name, "offset": info.Size()})
}

func (s *server) appendUpload(w http.ResponseWriter, r *http.Request, root, id string) {
	part, _, err := s.findPart(root, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	info, err := os.Stat(part)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if offset != info.Size() {
		// 偏移不匹配:告诉客户端当前真实的偏移,由它续传(幂等)。
		w.Header().Set("X-Upload-Offset", strconv.FormatInt(info.Size(), 10))
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "offset 与服务器不一致", "offset": info.Size()})
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxUploadChunk)
	file, err := os.OpenFile(part, os.O_WRONLY|os.O_APPEND, 0o660)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer file.Close()
	written, err := io.Copy(file, body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"upload_id": id, "offset": info.Size() + written})
}

func (s *server) cancelUpload(w http.ResponseWriter, r *http.Request, root, id string) {
	part, _, err := s.findPart(root, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	_ = os.Remove(part)
	writeJSON(w, http.StatusOK, map[string]string{"cancelled": id})
}

func (s *server) completeUpload(w http.ResponseWriter, r *http.Request, root, id string) {
	part, name, err := s.findPart(root, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	target := filepath.Join(root, name)
	onConflict := r.URL.Query().Get("on_conflict")

	if _, err := os.Stat(target); err == nil {
		switch onConflict {
		case "overwrite":
			// 继续往下走,rename 会原子替换
		case "rename":
			target = freeName(root, name)
		default:
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "同名文件已存在", "exists": true, "filename": name})
			return
		}
	}
	_ = os.Chmod(part, 0o660)
	if err := os.Rename(part, target); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	info, _ := os.Stat(target)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"filename": filepath.Base(target), "size": size, "path": target})
}

// cleanupUploads 删除卷里 .uploads 下的临时分片(删除卷时调用)。
func cleanupUploads(root string) error {
	return os.RemoveAll(filepath.Join(root, uploadDirName))
}

// sweepUploads 清理超过 maxAge 的残留分片(由定时任务调用,避免中断的上传长期占空间)。
func sweepUploads(root string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(filepath.Join(root, uploadDirName))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < maxAge {
			continue
		}
		if err := os.Remove(filepath.Join(root, uploadDirName, entry.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}
