#!/usr/bin/env python3
"""给 Coder 的 HDD 持久卷目录设置 XFS project quota。

设计:
- 卷 = 带标签 coder-hdd-volume=true 的 PVC(命名空间 coder-workspaces),
  由 cluster-capacity 服务(或手工 kubectl)创建;
- 卷目录 = PVC 注解 coder.com/hdd-volume-path(缺省由 PVC 名推导);
- 配额 = PVC 的 spec.resources.requests.storage → 容器内 df 看到的就是这个值,
  写超会被 XFS 内核直接拦下(和 SSD 上的工作区卷同一套机制);
- HDD 需要以 prjquota 挂载(/etc/fstab 已加,重启后生效);未生效时本脚本直接跳过。
"""
import json, os, re, subprocess, sys, zlib, pathlib

MOUNT = "__HDD_DATA__"
VOLROOT = "__VOLUME_ROOT__"
NAMESPACE = "__NAMESPACE__"
KUBECONFIG = "__ADMIN_KUBECONFIG__"
LABEL = "coder-hdd-volume=true"
DRY = "--dry-run" in sys.argv
QUIET = "--quiet" in sys.argv


def log(*a):
    if not QUIET:
        print(*a, flush=True)


def active_quota():
    try:
        opts = subprocess.run(["findmnt", "-no", "OPTIONS", MOUNT],
                              capture_output=True, text=True).stdout
    except FileNotFoundError:
        return False
    return "prjquota" in opts


def kubectl_json(args):
    env = dict(os.environ, KUBECONFIG=KUBECONFIG)
    r = subprocess.run(["kubectl", *args], capture_output=True, text=True, env=env)
    if r.returncode != 0:
        sys.exit(f"kubectl failed: {r.stderr.strip()}")
    return json.loads(r.stdout)


def to_bytes(s):
    m = re.fullmatch(r"(\d+)([KMGT]i?)", s)
    if not m:
        raise ValueError(s)
    n, u = int(m.group(1)), m.group(2)
    return n * {"K": 10**3, "M": 10**6, "G": 10**9, "T": 10**12,
                "Ki": 1024, "Mi": 1024**2, "Gi": 1024**3, "Ti": 1024**4}[u]


def project_id(name):
    return (zlib.crc32(name.encode()) % 4_000_000) + 100


def sweep_uploads(root, hours=24):
    """清掉中断上传留下的临时分片(超过 24 小时)。"""
    import time
    d = os.path.join(root, ".uploads")
    if not os.path.isdir(d):
        return 0
    deadline = time.time() - hours * 3600
    removed = 0
    for name in os.listdir(d):
        path = os.path.join(d, name)
        try:
            if os.path.getmtime(path) < deadline:
                os.remove(path)
                removed += 1
        except OSError:
            pass
    return removed


def xfs_quota(*args):
    cmd = ["xfs_quota", "-x", "-c", " ".join(args), MOUNT]
    if DRY:
        log("  [dry-run]", " ".join(cmd))
        return
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        log(f"  xfs_quota 失败: {r.stderr.strip() or r.stdout.strip()}")


def volumes():
    data = kubectl_json(["get", "pvc", "-n", NAMESPACE, "-l", LABEL, "-o", "json"])
    for item in data.get("items", []):
        meta = item["metadata"]
        name = meta["name"]
        ann = (meta.get("annotations") or {}).get("coder.com/hdd-volume-path")
        path = ann or os.path.join(VOLROOT, name[4:] if name.startswith("hdd-") else name)
        size = item["spec"]["resources"]["requests"]["storage"]
        yield name, path, size


def main():
    if not active_quota():
        log(f"{MOUNT} 未以 prjquota 挂载,跳过(重启后自动生效)")
        return
    pathlib.Path(VOLROOT).mkdir(parents=True, exist_ok=True)
    n = 0
    for name, path, size in volumes():
        pathlib.Path(path).mkdir(parents=True, exist_ok=True)
        pid = project_id(name)
        xfs_quota("project", "-s", "-p", path, str(pid))
        xfs_quota("limit", "-p", f"bhard={to_bytes(size)}", str(pid))
        stale = sweep_uploads(path)
        log(f"  {name}: {path} → {size} (project {pid})" + (f", 清理残留分片 {stale} 个" if stale else ""))
        n += 1
    log(f"共处理 {n} 个 HDD 卷")


if __name__ == "__main__":
    main()
