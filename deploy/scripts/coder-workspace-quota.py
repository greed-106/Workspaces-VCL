#!/usr/bin/env python3
"""给 Coder 工作区的持久卷目录设置 XFS project quota。

- 目录来源:local-path provisioner 建在 /mnt/ssd-data/local-path 下,
  命名形如 pvc-<uuid>_coder-workspaces_<pvc 名>
- 配额大小来源:PVC 的 spec.resources.requests.storage(= 用户在界面上申请的 home_disk_size)
- 因此容器内 df 看到的容量 = 用户申请值,写超会被内核直接拦下
"""
import glob, json, os, re, subprocess, sys, zlib

MOUNT = "/mnt/ssd-data"
BASE = "/mnt/ssd-data/local-path"
KUBECONFIG = "/etc/kubernetes/admin.conf"
DRY = "--dry-run" in sys.argv

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

def main():
    if not active_quota():
        print(f"{MOUNT} 未启用 prjquota,跳过(重启后自动生效)")
        return
    pvcs = kubectl_json(["get", "pvc", "-n", "coder-workspaces", "-o", "json"])["items"]
    sizes = {p["metadata"]["name"]: p["spec"]["resources"]["requests"]["storage"]
             for p in pvcs if p["spec"].get("volumeName")}
    done = 0
    for d in sorted(glob.glob(f"{BASE}/pvc-*_coder-workspaces_*")):
        m = re.search(r"_coder-workspaces_(.+)$", os.path.basename(d))
        if not m or not os.path.isdir(d):
            continue
        size = sizes.get(m.group(1))
        if not size:
            continue
        pid = project_id(os.path.basename(d))
        for cmd in (["xfs_quota", "-x", "-c", f"project -s -p {d} {pid}", MOUNT],
                    ["xfs_quota", "-x", "-c", f"limit -p bhard={to_bytes(size)} {pid}", MOUNT]):
            if DRY:
                print("DRY:", " ".join(cmd))
            else:
                r = subprocess.run(cmd, capture_output=True, text=True)
                if r.returncode != 0:
                    print(f"WARN: {' '.join(cmd)} -> {r.stderr.strip()}", file=sys.stderr)
        done += 1
        if not DRY:
            print(f"quota applied: {os.path.basename(d)} = {size}")
    print(f"处理了 {done} 个工作区卷(dry={DRY})")

main()
