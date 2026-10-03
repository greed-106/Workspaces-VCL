# Coder 多用户 GPU 工作区平台部署手册

本手册描述如何在一台 GPU 服务器上从裸机开始搭建按需分配 GPU、CPU、内存与磁盘的多用户工作区平台:底层是单节点 Kubernetes,控制面是跑在宿主机上的 Coder(coderd),工作区是 k8s 里的 Pod,容量与配额由本机服务与 XFS project quota 保证。文档里的路径、参数取值与命令来自本仓库 `deploy/` 下的配置文件以及目标机器的实际状态,按顺序复制执行即可;机器相关取值集中在 `deploy/local.env`(由 `deploy/local.env.example` 复制,已被 git 忽略),口令、令牌一律是占位符。

面向使用者的说明是另一份文档:`deploy/user-guide.md`(账号申请、改密码、数据卷、创建与修改工作区、数据存放位置、怎么确认自己拿到的资源),可以直接发给用户;模板自带的简短介绍在 `deploy/coder-template-kubernetes/README.md`,推送模板后会显示在 WebUI 的模板页面上。

## 一、这份文档怎么用

用 `$REPO` 表示本仓库的检出路径,开始前先设置:

```bash
export REPO=/data/mingjia/Workspaces-VCL
```

本仓库以 `/data/mingjia/Workspaces-VCL` 为唯一工作副本:Coder 源码、部署配置与构建产物都在这里,不再使用其它检出目录。控制面从本仓库构建与运行,systemd 单元的 `WorkingDirectory`、`ExecStart` 与 `--global-config` 都指向它。

部署配置里所有跟机器相关的取值(用户名、检出目录、访问地址、存储路径、节点名、容量参数)集中在 `deploy/local.env`,它由 `deploy/local.env.example` 复制而来,已被 git 忽略。仓库里的配置模板(`deploy/systemd/`、`deploy/scripts/`、`deploy/logrotate/`、`deploy/hdd-volumes/`)只写 `__占位符__`,占位符与 `deploy/local.env` 的键同名:

```bash
cd /data/mingjia/Workspaces-VCL
cp deploy/local.env.example deploy/local.env    # 首次;按本机情况修改
${EDITOR:-vi} deploy/local.env
sudo deploy/install.sh                          # 渲染到 deploy/generated/,并安装单元、脚本与 logrotate
sudo deploy/install.sh --render-only            # 只渲染,不改系统文件
```

`sudo deploy/install.sh` 把 systemd 单元装到 `/etc/systemd/system/`、配额脚本装到 `/usr/local/sbin/`、logrotate 装到 `/etc/logrotate.d/`,最后执行 `systemctl daemon-reload`。渲染出的卷 YAML 在 `deploy/generated/hdd-volumes/`,用 `kubectl apply` 应用。

仓库文件与安装位置的对应关系:

| 仓库路径 | 安装或使用位置 |
| --- | --- |
| `deploy/local.env.example` | 复制为 `deploy/local.env`(本机取值,git 忽略) |
| `deploy/install.sh` | 渲染 `deploy/generated/` 并安装单元、脚本与 logrotate |
| `deploy/systemd/*.service` 与 `*.timer` | 占位符模板,渲染后装到 `/etc/systemd/system/` |
| `deploy/scripts/*.py` | 占位符模板,渲染后装到 `/usr/local/sbin/`,权限 0755 |
| `deploy/logrotate/coder-dev` | 占位符模板,渲染后装到 `/etc/logrotate.d/coder-dev` |
| `deploy/hdd-volumes/*.yaml` | 占位符模板(StorageClass、占位卷与示例卷),渲染到 `deploy/generated/hdd-volumes/` |
| `deploy/generated/` | 渲染产物:`systemd/`、`scripts/`、`logrotate/`、`hdd-volumes/` |
| `deploy/cluster-capacity/` | 容量服务源码,`go build` 后装到 `/usr/local/bin/cluster-capacity` |
| `deploy/coder-template-kubernetes/` | Coder 模板目录(`main.tf` 与 `scripts/check-capacity.sh`) |
| `deploy/workspace-image/` | 工作区镜像构建上下文(`Dockerfile` 与 `build.sh`) |
| `deploy/gpu-operator-values.yaml` | GPU Operator 的 Helm values |
| `deploy/kube-flannel-patched.yml` | Flannel manifest,已含网卡参数 |
| `deploy/design/hdd-volumes.md` | HDD 数据卷设计说明 |
| `deploy/CHANGES-FROM-UPSTREAM.md` | 相对 Coder 上游的前端改动清单 |

## 二、项目概览与架构

```text
浏览器 http://<域名>/(80,nginx 反向代理)
   |
   |  nginx:按域名分流,WebSocket 与流式传输直通,详见 14.7
   v
Coder 控制面 coderd(127.0.0.1:3001,API + Web UI + 工作区代理),内嵌 PostgreSQL,指标 :2114
   |
工作区 Pod,命名空间 coder-workspaces
   |  Deployment,镜像内置 code-server(浏览器版 VS Code,Pod 内 127.0.0.1:13337)
   |  工作区 PVC(local-path,数据落 /mnt/ssd-data/local-path)
   |  HDD 数据卷 PVC(coder-hdd,数据落 /mnt/hdd-data/volumes,挂到容器内 /mnt/data)
   v
单节点 Kubernetes v1.37.1:kubeadm + containerd + Flannel + local-path
   |  GPU 由 GPU Operator 提供,nvidia.com/gpu 调度,runtime_class_name=nvidia
   v
宿主机:10 张 RTX 3080 Ti、48 核、377 GiB 内存、XFS project quota 限容
容量服务 cluster-capacity(:3999)读 k8s 与 XFS,向页面提供实时余量
```

关键事实:

- 控制面跑在宿主机,不在 Pod 里;模板用 `use_kubeconfig = true` 读宿主 `~/.kube/config`,provisioner 也在这台机器上。
- 工作区是普通 Deployment,限制靠 `limits`;镜像与容器可写层都在 SSD,不占 40G 根分区。
- 每个工作区一个 PVC(`coder-<工作区 id>-home`),用 `subPath` 挂到 `/home/coder`、`/usr`、`/etc`、`/opt`、`/var/lib`、`/var/cache`,首次启动由 initContainer 把镜像内容拷进空卷。
- HDD 冷数据卷在工作区之外创建(PV 与 PVC),删掉工作区不影响卷里的数据,可反复挂到不同工作区。

端口:80 对外入口(nginx,按域名转发到控制面),3999 对外入口(nginx,转发到容量与数据卷服务);控制面本机监听 127.0.0.1:3001,容量服务本机监听 127.0.0.1:3998,2114 指标,6443 kube-apiserver,13337 工作区内的 code-server(仅监听 Pod 内 127.0.0.1,经 Coder 隧道访问)。7080 与 3010 属于 `scripts/develop.sh` 的开发模式(前端 dev server 与工作区代理),日常工作不用;3000 是本机被其它服务占用的端口,控制面不使用。

## 三、硬件与软件前提

硬件(本部署实测规格):

| 项 | 值 |
| --- | --- |
| CPU 与内存 | 48 个逻辑核,377 GiB |
| 系统盘 | `/dev/nvme0n1`,根分区 40G |
| SSD | `/dev/sde`(3.6T)+ `/dev/sdf`(1.9T)+ `/dev/sdg`(1.9T),LVM `ssd-vg/ssd-data` = 7.36T,XFS |
| HDD | `/dev/sda` 到 `/dev/sdd`(4 × 1.8T ST2000NX0423),LVM `hdd-vg/hdd-data` = 7.28T,XFS |
| GPU | 10 × NVIDIA GeForce RTX 3080 Ti(12GB),`/dev/nvidia0` 到 `/dev/nvidia9` |
| 网卡 | `eno1` 为业务网卡(1GbE),另有 `eno2` 与一张 USB 网卡,无 IB 与 RoCE |

两个 LVM 卷都是 linear 型、跨多块盘,没有冗余:任意一块盘故障会丢整个卷的数据。

软件版本:

| 组件 | 版本 |
| --- | --- |
| 操作系统 | Ubuntu 24.04.5,kernel 6.8.0-142-generic |
| containerd | 2.2.1(Ubuntu 包 `containerd`,`SystemdCgroup = true`) |
| Kubernetes | v1.37.1,kubeadm / kubelet / kubectl 已 `apt-mark hold` |
| CNI 与存储 | Flannel v0.28.9(vxlan,Pod 网段 `10.244.0.0/16`)、local-path-provisioner v0.0.30 |
| GPU | GPU Operator chart v26.7.1、驱动 595.91.07(`kernelModuleType: open`)、CUDA 13.2 |
| 其他 | Helm v3.19.0(`/usr/local/bin/helm`)、Go 1.26.5、Node v24.21.0、pnpm、podman 4.9.3、protoc 3.21.12、pg_dump 16.15 |
| Terraform provider | coder 2.18.0、kubernetes 3.2.1,预置在 `~/.terraform.d/plugins`;集群凭据 `/data/mingjia/.kube/config`,权限 600 |

网络可达性,决定了全部下载来源:`registry.k8s.io`、`registry-1.docker.io`、`auth.docker.io`、`raw.githubusercontent.com` 不可达;`quay.io`、`ghcr.io`、`registry.aliyuncs.com`、`k8s.m.daocloud.io`、`docker.m.daocloud.io`、`docker.1panel.live`、`pkgs.k8s.io`、`nvidia.github.io`、`nvcr.io`、`helm.ngc.nvidia.com`、`goproxy.cn` 可达。GitHub 资源统一用 `https://ghproxy.net/` 前缀下载。

账号与目录:管理员账号 `mingjia`,家目录 `/data/mingjia`,需要免密 sudo。`/data` 与根分区是同一块 40G 盘,构建与镜像一律放 `/mnt/ssd-data`。工作区容器内统一使用镜像自带的 `coder` 账号(uid/gid 1000)。

## 四、步骤一:磁盘布局与 XFS 项目配额

建立 LVM 卷并格式化:

```bash
sudo pvcreate /dev/sde /dev/sdf /dev/sdg
sudo vgcreate ssd-vg /dev/sde /dev/sdf /dev/sdg
sudo lvcreate -l 100%FREE -n ssd-data ssd-vg

sudo pvcreate /dev/sda /dev/sdb /dev/sdc /dev/sdd
sudo vgcreate hdd-vg /dev/sda /dev/sdb /dev/sdc /dev/sdd
sudo lvcreate -l 100%FREE -n hdd-data hdd-vg

sudo mkfs.xfs /dev/ssd-vg/ssd-data
sudo mkfs.xfs /dev/hdd-vg/hdd-data
```

`prjquota` 必须在挂载时带上,XFS 不支持 `mount -o remount,prjquota` 事后开启。写入 `/etc/fstab`:

```text
/dev/hdd-vg/hdd-data /mnt/hdd-data xfs defaults,noatime,nofail,prjquota 0 0
/dev/ssd-vg/ssd-data /mnt/ssd-data xfs defaults,noatime,nofail,prjquota 0 0
```

挂载、验收并建目录:

```bash
sudo mkdir -p /mnt/ssd-data /mnt/hdd-data
sudo mount -a
findmnt -no OPTIONS /mnt/ssd-data | grep -o prjquota
findmnt -no OPTIONS /mnt/hdd-data | grep -o prjquota

sudo mkdir -p /mnt/ssd-data/{containerd,local-path,tmp,logs,mingjia-caches}
sudo mkdir -p /mnt/hdd-data/volumes
sudo chown -R mingjia:mingjia /mnt/ssd-data/{tmp,logs,mingjia-caches}
sudo chown mingjia:mingjia /mnt/hdd-data/volumes
```

`/mnt/hdd-data/volumes` 必须属于运行 cluster-capacity 的用户,否则服务建卷目录时会 permission denied。

控制根分区增长:在 `/etc/systemd/journald.conf` 里设 `SystemMaxUse=500M` 并重启 systemd-journald;`/etc/logrotate.d/rsyslog` 用 `size 100M`、`rotate 4`。验收用 `df -h / /mnt/ssd-data /mnt/hdd-data`。

## 五、步骤二:单节点 Kubernetes

节点名 `ubuntu0002`,IP `10.129.164.15`;换机器时替换这两项。

### 5.1 系统准备

```bash
sudo swapoff -a
sudo sed -i '/ swap / s/^/#/' /etc/fstab

sudo tee /etc/modules-load.d/k8s.conf >/dev/null <<'EOF'
overlay
br_netfilter
EOF
sudo modprobe overlay br_netfilter

sudo tee /etc/sysctl.d/k8s.conf >/dev/null <<'EOF'
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sudo sysctl --system
```

### 5.2 containerd

```bash
sudo apt-get update
sudo apt-get install -y containerd
sudo mkdir -p /etc/containerd/conf.d
sudo containerd config default | sudo tee /etc/containerd/config.toml >/dev/null
sudo sed -i 's|^imports = .*|imports = ["/etc/containerd/conf.d/*.toml"]|' /etc/containerd/config.toml
sudo sed -i 's|^root = .*|root = "/mnt/ssd-data/containerd"|' /etc/containerd/config.toml
sudo sed -i 's|SystemdCgroup = false|SystemdCgroup = true|' /etc/containerd/config.toml
sudo sed -i -E 's|^(\s*)sandbox = .*|\1sandbox = "registry.aliyuncs.com/google_containers/pause:3.10.2"|' /etc/containerd/config.toml
sudo systemctl restart containerd
```

| 配置 | 值 | 作用 |
| --- | --- | --- |
| `root` | `/mnt/ssd-data/containerd` | 镜像与快照落 SSD;`state` 保持 `/run/containerd` |
| `SystemdCgroup` | `true` | 与 kubelet 的 cgroup 驱动一致 |
| `pinned_images.sandbox` | `registry.aliyuncs.com/google_containers/pause:3.10.2` | 默认值是拉不动的 `registry.k8s.io/pause:3.10.1`;kubelet 1.35 起不再提供 pause 镜像,只能由 containerd 指定 |
| `imports` | `/etc/containerd/conf.d/*.toml` | containerd 2.2.1 的默认配置已含该 import,这条 sed 保证它存在,后面的 GPU Operator 用它注入运行时 |

验证:

```bash
grep -nE '^(imports|root|state)|SystemdCgroup|sandbox =' /etc/containerd/config.toml
sudo ctr -n k8s.io images pull registry.aliyuncs.com/google_containers/pause:3.10.2
```

### 5.3 kubelet、kubeadm、kubectl

```bash
sudo apt-get install -y apt-transport-https ca-certificates curl gpg
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.37/deb/Release.key | \
  sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.37/deb/ /' | \
  sudo tee /etc/apt/sources.list.d/kubernetes.list
sudo apt-get update
sudo apt-get install -y kubelet kubeadm kubectl
sudo apt-mark hold kubelet kubeadm kubectl
```

### 5.4 DNS

宿主机 `/etc/resolv.conf` 指向 systemd-resolved 的回环桩 `127.0.0.53`,kubelet 拒绝回环 DNS,因此单独给 kubelet 一份解析配置:

```bash
sudo tee /etc/k8s-resolv.conf >/dev/null <<'EOF'
nameserver 162.105.129.122
nameserver 162.105.129.88
options timeout:2 attempts:3
EOF
echo 'KUBELET_EXTRA_ARGS="--resolv-conf=/etc/k8s-resolv.conf"' | sudo tee /etc/default/kubelet
sudo systemctl restart kubelet
```

### 5.5 初始化集群

```bash
sudo kubeadm init \
  --image-repository=registry.aliyuncs.com/google_containers \
  --apiserver-advertise-address=10.129.164.15 \
  --pod-network-cidr=10.244.0.0/16 \
  --cri-socket=unix:///run/containerd/containerd.sock

mkdir -p /data/mingjia/.kube
sudo cp /etc/kubernetes/admin.conf /data/mingjia/.kube/config
sudo chown mingjia:mingjia /data/mingjia/.kube/config
chmod 600 /data/mingjia/.kube/config
export KUBECONFIG=/data/mingjia/.kube/config
kubectl get --raw='/healthz'

kubectl taint nodes ubuntu0002 node-role.kubernetes.io/control-plane:NoSchedule-
```

最后一条移除 control-plane 污点,单节点不移除的话业务 Pod 无法调度。以后加节点时在主节点生成 join 命令(token 24 小时过期):`sudo kubeadm token create --print-join-command`。

### 5.6 Flannel 与 local-path

镜像都来自可达的 `ghcr.io` 与 `docker.m.daocloud.io`。`deploy/kube-flannel-patched.yml` 相对上游只多一个 `--iface=eno1` 参数:机器有多张网卡,不指定时 Flannel 可能选错。

```bash
export KUBECONFIG=/data/mingjia/.kube/config
kubectl apply -f "$REPO/deploy/kube-flannel-patched.yml"

curl -fsSL -o /tmp/local-path-storage.yaml \
  "https://ghproxy.net/https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.30/deploy/local-path-storage.yaml"
sed -i 's|rancher/local-path-provisioner:v0.0.30|docker.m.daocloud.io/rancher/local-path-provisioner:v0.0.30|' /tmp/local-path-storage.yaml
kubectl apply -f /tmp/local-path-storage.yaml
kubectl -n local-path-storage patch configmap local-path-config --type merge -p \
  '{"data":{"config.json":"{\n  \"nodePathMap\":[\n    {\n      \"node\":\"DEFAULT_PATH_FOR_NON_LISTED_NODES\",\n      \"paths\":[\"/mnt/ssd-data/local-path\"]\n    }\n  ]\n}\n"}}'
kubectl -n local-path-storage rollout restart deploy/local-path-provisioner
```

StorageClass 是 `WaitForFirstConsumer`,PVC 要等 Pod 被调度后才 Bound,Pending 属于正常中间态。Flannel v0.28.9 不执行 NetworkPolicy,静默忽略;要按工作区建立网络边界必须换 Calico 或 Cilium。

### 5.7 验收

```bash
export KUBECONFIG=/data/mingjia/.kube/config
kubectl get nodes -o wide
kubectl get pods -A
kubectl get sc
kubectl run nettest --image=docker.m.daocloud.io/library/nginx:latest --restart=Never
kubectl get pod nettest -o wide
kubectl delete pod nettest
```

## 六、步骤三:NVIDIA 驱动与 GPU Operator

本部署由 GPU Operator 全权管理驱动与容器工具链:主机不装发行版 NVIDIA 驱动包,驱动以容器形式安装到 `/run/nvidia/driver`。

### 6.1 主机前提

```bash
mokutil --sb-state                    # 需要 Secure Boot 关闭
dpkg -l 'linux-headers-*'             # 需要与当前内核对应的头文件
printf 'blacklist nouveau\noptions nouveau modeset=0\n' | \
  sudo tee /etc/modprobe.d/blacklist-nouveau-k8s.conf
sudo update-initramfs -u -k all
sudo ctr -n k8s.io images pull nvcr.io/nvidia/driver:595.91.07-ubuntu24.04
```

Docker Hub 不可达,所有镜像都走可达镜像源;`nvcr.io` 可达,驱动镜像直接从那里拉。

### 6.2 安装

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update
helm install gpu-operator nvidia/gpu-operator \
  -n gpu-operator --create-namespace \
  -f "$REPO/deploy/gpu-operator-values.yaml"
```

`deploy/gpu-operator-values.yaml` 的关键取值:

| 键 | 值 | 原因 |
| --- | --- | --- |
| `driver.enabled` | `true` | 驱动由 Operator 的容器化驱动提供 |
| `driver.kernelModuleType` | `open` | Ampere 架构支持,也是 NVIDIA 560 系列之后的默认方向 |
| `driver.version` 与 `upgradePolicy.autoUpgrade` | `595.91.07` 与 `false` | 锁死驱动版本,禁止自动升级 |
| `toolkit.enabled` | `true` | 工具链由 Operator 的 DaemonSet 提供,装在 `/usr/local/nvidia/toolkit` |
| `node-feature-discovery.image` | `k8s.m.daocloud.io/nfd/node-feature-discovery:v0.19.0` | `registry.k8s.io` 不可达 |
| `cdi.enabled` | `true` | 与 containerd 的 `enable_cdi = true` 对应 |

values 里的路径是子 chart 名 `node-feature-discovery`,写成 `nfd.image.*` 不生效。

### 6.3 主机侧驱动库链接

容器化驱动的库只在 `/run/nvidia/driver/usr/lib/x86_64-linux-gnu/` 下,而 `nvidia-container-runtime` 按需生成 CDI 规范时跑在主机上,需要主机的动态链接器能找到 NVML。驱动 DaemonSet 起来之后执行:

```bash
sudo ls /run/nvidia/driver/usr/lib/x86_64-linux-gnu | wc -l     # 应为数百条
for lib in $(sudo ls /run/nvidia/driver/usr/lib/x86_64-linux-gnu/ | grep -E '^lib(nvidia|cuda)'); do
  sudo ln -sf "/run/nvidia/driver/usr/lib/x86_64-linux-gnu/$lib" "/usr/lib/x86_64-linux-gnu/$lib"
done
sudo ldconfig
ls /usr/lib/x86_64-linux-gnu | grep -cE '^lib(nvidia|cuda)'
```

`.so` 与 `.so.1` 这类版本无关链接在驱动升级后依然有效。

### 6.4 运行时与 RuntimeClass

Operator 的 toolkit DaemonSet 生成 containerd 的 drop-in `/etc/containerd/conf.d/99-nvidia.toml`,注册三个运行时并开启 CDI:`enable_cdi = true`、`cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]`、`default_runtime_name = "nvidia"`、`runtimes.nvidia.options.BinaryName = /usr/local/nvidia/toolkit/nvidia-container-runtime`(另有 `nvidia-cdi` 与 `nvidia-legacy`)、`SystemdCgroup = true`。主配置 `/etc/containerd/config.toml` 的 `pinned_images.sandbox` 不受 drop-in 影响,仍是阿里云源。

工作区通过 RuntimeClass 拿到驱动库注入。`kubectl get runtimeclass` 应能看到 `nvidia`;若缺失,补建:

```bash
kubectl create -f - <<< '{"apiVersion":"node.k8s.io/v1","kind":"RuntimeClass","metadata":{"name":"nvidia"},"handler":"nvidia"}'
```

### 6.5 验收

```bash
export KUBECONFIG=/data/mingjia/.kube/config
kubectl get clusterpolicy                            # STATUS 应为 ready
kubectl -n gpu-operator get pods                     # 全部 Running 或 Completed
kubectl get node ubuntu0002 -o jsonpath='{.status.allocatable.nvidia\.com/gpu}'; echo   # 10
kubectl get node ubuntu0002 -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}'; echo

kubectl run gpu-alloc-test --restart=Never --image=nvcr.io/nvidia/cuda:12.9.0-base-ubuntu24.04 \
  --overrides='{"spec":{"containers":[{"name":"cuda","image":"nvcr.io/nvidia/cuda:12.9.0-base-ubuntu24.04","command":["nvidia-smi","-L"],"resources":{"limits":{"nvidia.com/gpu":1}}}]}}'
kubectl logs gpu-alloc-test          # 只应列出 1 张卡
kubectl delete pod gpu-alloc-test
```

申请几张就看到几张,不要指望省略 `limits` 也能拿到卡。多卡训练时 `/dev/shm` 默认只有 64MiB,必须给 Pod 挂内存盘:

```yaml
volumeMounts:
- {name: dshm, mountPath: /dev/shm}
volumes:
- {name: dshm, emptyDir: {medium: Memory, sizeLimit: 8Gi}}
```

### 6.6 重启行为

`/run` 是 tmpfs,`/run/nvidia/driver` 不跨重启保留,所以每次重启后驱动容器会重新安装一遍驱动(约 2 到 4 分钟),期间 GPU 不可用,之后 Operator 会依次拉起工具链、device plugin、GFD 与 DCGM。6.3 节的链接由 `/etc` 保存,重启后自动指向新装好的驱动。

## 七、步骤四:Coder 控制面

控制面用当前 checkout 的源码构建,以 systemd 服务常驻,自带内嵌 PostgreSQL。

### 7.1 构建依赖

```bash
sudo apt-get install -y make build-essential jq protobuf-compiler postgresql-client
REV=337309bfb9524f38466a5090e310040fc7af0203
curl -sSLf -o /tmp/sqlc.tar.gz "https://ghproxy.net/https://github.com/coder/sqlc/archive/$REV.tar.gz"
mkdir -p /tmp/sqlc-src && tar -xzf /tmp/sqlc.tar.gz -C /tmp/sqlc-src
cd /tmp/sqlc-src/sqlc-$REV
export PATH=/usr/local/go/bin:/mnt/ssd-data/mingjia-caches/go/bin:$PATH
export GOPATH=/mnt/ssd-data/mingjia-caches/go GOCACHE=/mnt/ssd-data/mingjia-caches/go-build
export GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=local TMPDIR=/mnt/ssd-data/tmp
go install ./cmd/sqlc
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest storj.io/drpc/cmd/protoc-gen-go-drpc@latest
```

SQLC 必须用 `coder/sqlc` fork(修了上游 ambiguous column 问题),revision 与仓库 `flake.nix` 里 pin 的一致。

### 7.2 构建

```bash
cd /data/mingjia/Workspaces-VCL
export TMPDIR=/mnt/ssd-data/tmp GOTMPDIR=/mnt/ssd-data/tmp MAKEFLAGS='OS_ARCHES=linux_amd64'
export PATH=/usr/local/go/bin:/mnt/ssd-data/mingjia-caches/go/bin:/data/mingjia/.local/share/fnm/node-versions/v24.21.0/installation/bin:$PATH
export GOPATH=/mnt/ssd-data/mingjia-caches/go GOCACHE=/mnt/ssd-data/mingjia-caches/go-build
export GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=local
make site/out/index.html build/coder_linux_amd64 build/coder-slim_linux_amd64
```

`TMPDIR` 与 `MAKEFLAGS` 必须设置:根分区只有 40G,交叉编译七个平台会写满它。

### 7.3 首次启动与管理员账号

```bash
cd /data/mingjia/Workspaces-VCL
export TMPDIR=/mnt/ssd-data/tmp CODER_DEV_SKIP_SETUP=true
export CODER_DEV_PORT=3001 CODER_DEV_WEB_PORT=7080 CODER_DEV_PROXY_PORT=3010 CODER_DEV_PROMETHEUS_PORT=2114
export CODER_DEV_ACCESS_URL=http://10.129.164.15:3001
./scripts/develop.sh
```

`CODER_DEV_SKIP_SETUP=true` 会连首个用户创建一起跳过,用 API 建管理员,并把 session token 写进 CLI 使用的配置目录(`scripts/coder-dev.sh` 固定用 `./.coderv2`):

```bash
curl -X POST http://127.0.0.1:3001/api/v2/users/first -H 'Content-Type: application/json' \
  -d '{"email":"admin@coder.com","username":"admin","name":"Admin User","password":"<管理员口令>"}'

cd /data/mingjia/Workspaces-VCL
TOKEN=$(curl -sS -X POST http://127.0.0.1:3001/api/v2/users/login -H 'Content-Type: application/json' \
  -d '{"email":"admin@coder.com","password":"<管理员口令>"}' | jq -r .session_token)
mkdir -p ./.coderv2
printf '%s' "$TOKEN" > ./.coderv2/session
printf '%s' 'http://127.0.0.1:3001' > ./.coderv2/url

./build/coder-slim_linux_amd64 --global-config ./.coderv2 list
./build/coder-slim_linux_amd64 --global-config ./.coderv2 users create \
  --username alice --email alice@coder.local --full-name Alice --password '<用户口令>'
```

### 7.4 systemd 常驻

```bash
cd "$REPO"
sudo deploy/install.sh
sudo systemctl enable --now coder-dev
systemctl status coder-dev
```

单元模板在 `deploy/systemd/coder-dev.service`,渲染结果在 `deploy/generated/systemd/coder-dev.service`。单元要点:`User=__CODER_USER__`、`WorkingDirectory=__CODER_DIR__`、`Environment=HOME=__CODER_HOME__`、`Environment=TMPDIR=__TMPDIR__`、`After=network-online.target kubelet.service`、`RequiresMountsFor=__SSD_DATA__`、`Restart=always` 与 `RestartSec=10`,失败上限 `StartLimitBurst=3` / `StartLimitIntervalSec=120`;本机渲染后是 `User=mingjia`、`WorkingDirectory=/data/mingjia/Workspaces-VCL`、`TMPDIR=/mnt/ssd-data/tmp`、`RequiresMountsFor=/mnt/ssd-data`。`ExecStart` 的关键参数(取值来自 `deploy/local.env`):

| 参数 | 作用 |
| --- | --- |
| `--http-address __HTTP_ADDR__` | 监听地址(`0.0.0.0:3001`) |
| `--access-url __ACCESS_URL__` | 浏览器与工作区使用的访问地址,必须写节点 IP |
| `--dangerous-allow-cors-requests=true` | 把 coderd API 的 CORS 头设为 `*`,上游默认只放行同一用户的工作区 app 之间的跨域;开发实例也带这个参数 |
| `--dangerous-allow-path-app-sharing=true` | 允许共享基于路径的 app;不开时这类 app 的共享级别会被强制降为 `owner`,非属主打不开网页 VS Code |
| `--prometheus-enable --prometheus-address 0.0.0.0:2114` | 指标 |
| `--swagger-enable`、`--enable-terraform-debug-mode` | API 文档与 Terraform 调试输出 |

日志路径由 `__LOG_DIR__` 决定,本机写到 `/mnt/ssd-data/logs/coderd.log`,轮转配置 100M × 4、`copytruncate`(模板 `deploy/logrotate/coder-dev`)。

### 7.5 日常操作

```bash
systemctl restart coder-dev                    # 重建代码后重启
journalctl -u coder-dev -n 50                  # 单元日志
tail -f /mnt/ssd-data/logs/coderd.log          # 应用日志
```

用 `./scripts/develop.sh` 之前必须先 `sudo systemctl stop coder-dev`,两者都占用 3001。推送模板时不要用 `scripts/coder-dev.sh`:它每次都跑 `make`,而 `make` 生成 `coderd/database/dump.sql` 时会启动自己的 PostgreSQL,与运行中的控制面抢数据库;直接调二进制即可。

## 八、步骤五:工作区镜像

两个镜像共用同一份 `Dockerfile`,通过 `--build-arg BASE_IMAGE` 切换基础镜像,都内置固定版本的 code-server。

| 模板选项 | 镜像 | 基础镜像 |
| --- | --- | --- |
| `Ubuntu-24.04`(默认) | `coder-workspace:ubuntu24.04-code-server-4.139.1` | `docker.1panel.live/library/ubuntu:24.04` |
| `Ubuntu-24.04(CUDA 12.8)` | `coder-workspace:ubuntu24.04-cuda12.8-code-server-4.139.1` | `docker.1panel.live/nvidia/cuda:12.8.1-devel-ubuntu24.04` |

`Dockerfile` 的要点:

- `ARG CODE_SERVER_VERSION=4.139.1` 与对应的 `CODE_SERVER_SHA256`,构建时校验压缩包;apt 源替换为 `cn.archive.ubuntu.com`,并安装 code-server 需要的图形库(`libnss3`、`libgbm1`、`libasound2t64` 等)与常用工具。
- 统一运行身份为 `coder`(uid/gid 1000):官方 CUDA 镜像里 uid 1000 是 `ubuntu`,构建时改名并把家目录迁到 `/home/coder`,同时写免密 sudo 规则 `/etc/sudoers.d/coder`。
- 不预装任何 VS Code 插件,插件与设置落在持久化的 `/home/coder`。

构建与导入:

```bash
cd "$REPO/deploy/workspace-image"
./build.sh          # 两个都构建
./build.sh base     # 只重建基础版
./build.sh cuda     # 只重建 CUDA 版

BASE_CUDA=nvcr.io/nvidia/cuda:12.8.1-devel-ubuntu24.04 ./build.sh cuda   # 国内源拉不动 CUDA 基础镜像时
```

`build.sh` 做四件事:下载 code-server(GitHub release 直连超时,默认走 `https://ghproxy.net/` 前缀,下完校验 sha256 与 gzip 完整性)、`sudo podman build --format docker`、用管道导入 containerd 的 `k8s.io` 命名空间(不落临时文件)、补打 `docker.io/library/...` 标签(kubelet 会把短镜像名规范化成这个名字)。

```bash
sudo ctr -n k8s.io images ls -q | grep 'coder-workspace:'
sudo podman run --rm coder-workspace:ubuntu24.04-code-server-4.139.1 code-server --version
```

CUDA 版基于 devel 镜像,不含 python3;需要时在工作区里 `sudo apt install python3 python3-pip`,或改进 `Dockerfile` 后重建。

## 九、步骤六:工作区模板

模板目录 `deploy/coder-template-kubernetes/`,含 `main.tf` 与 `scripts/check-capacity.sh`。

### 9.1 模板变量

| 变量 | 取值 | 说明 |
| --- | --- | --- |
| `use_kubeconfig` | `true` | provisioner 读宿主 `~/.kube/config` |
| `namespace` | `coder-workspaces` | 工作区命名空间,需预先存在 |
| `storage_capacity_gb` | `6800` | 磁盘预算,与容量服务 `-disk-budget-gb` 一致 |
| `cpu_overcommit` | `2` | CPU 超分倍数,与容量服务 `-cpu-overcommit` 一致 |
| `system_reserve_cpu` | `4` | 系统预留核数 |
| `system_reserve_mem_gb` | `16` | 系统预留内存 |
| `workspace_image` | 声明但模板中未引用 | 镜像实际由参数 `image` 的选项决定 |

### 9.2 参数取值表

参数按 `order` 显示,全部不带图标:

| order | 参数 | 类型 | 默认 | 界面取值 | mutable | 落到 Pod 上的效果 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | `cpu` | 选项 | 8 | 8 / 16 / 32 | 是 | `limits.cpu` 与 `OMP_NUM_THREADS` |
| 2 | `memory` | 选项 | 16 | 16 / 32 / 48 | 是 | `limits.memory` |
| 3 | `gpu_model` | 选项 | `none` | 不使用显卡 + 探测到的型号 | 是 | `node_selector: nvidia.com/gpu.product` |
| 4 | `gpu_count` | 数字 | 1 | 界面 1 到 4 | 是 | `limits."nvidia.com/gpu"` |
| 5 | `image` | 选项 | 基础版 | 两个选项 | 否 | 容器镜像 |
| 6 | `home_disk_size` | 数字 | 100 | 界面 100 到 500,步长 100 | 否 | PVC `requests.storage` |
| 7 | `hdd_volume` | 字符串 | 空 | 卷的 PVC 名 | 是 | 挂载该 PVC 到 `/mnt/data`,留空挂占位卷 `hdd-none` |

`cpu` 与 `memory` 的选项在每次 plan 时按实时空闲过滤。`gpu_count` 与 `home_disk_size` 故意用自由数字而不是选项列表:选项会被服务端强制,那样 CLI 与 API 就无法申请范围外的值。

### 9.3 模板关键实现

| 位置 | 做法 |
| --- | --- |
| 容器 | `runtime_class_name = "nvidia"`,由 GPU Operator 注入驱动库 |
| 主机名 | `pku-vcl-<工作区 id 的 md5 前 4 位>`,创建后固定 |
| 安全上下文 | Pod 级 `run_as_user = 1000`、`fs_group = 1000`、`run_as_non_root = true` |
| 持久化 | 一个 PVC 用 `subPath` 挂到 `/home/coder`、`/usr`、`/etc`、`/opt`、`/var/lib`、`/var/cache`;不持久化 `/tmp`、`/var/run`、`/proc`、`/sys`、`/dev`、`/root` |
| 首次拷贝 | initContainer `init-persist` 以 root 运行,把镜像里对应目录拷进空卷并写标记 `/persist/.coder-init/<路径>`,之后启停不重复拷贝 |
| 临时目录 | `/tmp` 与 `/var/tmp` 用 `emptyDir: {medium: Memory, sizeLimit: 8Gi}`,容量受内存限额约束 |
| 容量把关 | `terraform_data.capacity_check` 调 `scripts/check-capacity.sh`,比较 CPU、内存、磁盘、GPU 四项 |
| 部署等待 | `wait_for_rollout = true`,资源不足时构建直接失败,而不是给一个永远 Pending 的工作区 |

### 9.4 推送模板

`coder/coder` provider 托管在 GitHub Releases,本网络不可达,已预置到 `~/.terraform.d/plugins/registry.terraform.io/{coder/coder/2.18.0,hashicorp/kubernetes/3.2.1}/linux_amd64/`。推送前先做语法自检:

```bash
rm -rf /tmp/tfval && mkdir -p /tmp/tfval
cp "$REPO/deploy/coder-template-kubernetes/main.tf" /tmp/tfval/
cd /tmp/tfval
/data/mingjia/.cache/coder/provisioner-0/tf/terraform init -input=false -no-color >/dev/null
/data/mingjia/.cache/coder/provisioner-0/tf/terraform validate -no-color
```

首次创建与后续更新:

```bash
cd /data/mingjia/Workspaces-VCL
./build/coder-slim_linux_amd64 --global-config ./.coderv2 templates create kubernetes \
  --directory "$REPO/deploy/coder-template-kubernetes" \
  --variable use_kubeconfig=true --variable namespace=coder-workspaces --yes
./build/coder-slim_linux_amd64 --global-config ./.coderv2 templates push kubernetes \
  --directory "$REPO/deploy/coder-template-kubernetes" --yes
```

`gpu_model` 的型号与显存是推送模板那一刻从节点标签读出来的,加卡或换型号后需要重新推送。

`deploy/coder-template-kubernetes/README.md` 是模板自带的介绍:推送后它会显示在 WebUI 的模板页面上,给用户说明这个模板能拿到什么资源、怎么申请、数据卷怎么用。改完这个文件也要重新推送模板才会生效,内容按面向使用者的口径写,不要写内部路径。

### 9.5 用 API 建工作区

非 TTY 环境下 CLI 会卡在参数交互,用 API 最稳:

```bash
curl -X POST "http://127.0.0.1:3001/api/v2/users/<user_id>/workspaces" \
  -H "Coder-Session-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"template_id":"<模板 id>","name":"alice-gpu-dev","rich_parameter_values":[
        {"name":"cpu","value":"8"},{"name":"memory","value":"16"},
        {"name":"gpu_model","value":"NVIDIA-GeForce-RTX-3080-Ti"},
        {"name":"gpu_count","value":"1"},{"name":"home_disk_size","value":"100"}]}'
```

`template_id` 与 `template_version_id` 互斥,只能给一个。把运行中的工作区切到新模板版本必须先停再启,只发 `start` 是空操作:

```bash
curl -X POST "http://127.0.0.1:3001/api/v2/workspaces/<id>/builds" -H "Coder-Session-Token: $TOKEN" \
  -H 'Content-Type: application/json' -d '{"transition":"stop"}'
curl -X POST "http://127.0.0.1:3001/api/v2/workspaces/<id>/builds" -H "Coder-Session-Token: $TOKEN" \
  -H 'Content-Type: application/json' -d '{"transition":"start","template_version_id":"<新版本 id>"}'
```

## 十、步骤七:磁盘配额

机制:PVC 的 `spec.resources.requests.storage` 就是要给用户的容量;定时脚本按 PVC 名找到 local-path 建出的目录,用 XFS project quota 设硬上限;容器里 `df` 因此显示申请值而不是整盘 7.4T。

```bash
cd "$REPO"
sudo deploy/install.sh
sudo systemctl enable --now coder-workspace-quota.timer coder-hdd-volume-quota.timer
```

两个脚本由 `deploy/install.sh` 按 `deploy/local.env` 渲染后装到 `/usr/local/sbin/`(模板里是 `__ADMIN_KUBECONFIG__`、`__SSD_DATA__`、`__HDD_DATA__`、`__VOLUME_ROOT__`、`__NAMESPACE__` 这些占位符),都以 root 运行、用 `KUBECONFIG=/etc/kubernetes/admin.conf` 调 kubectl,内部先检查挂载选项里有没有 `prjquota`,没有就跳过。

`coder-workspace-quota.py` 扫描 `/mnt/ssd-data/local-path/pvc-*_coder-workspaces_*`,按 PVC 申请值设配额;`coder-hdd-volume-quota.py` 处理带标签 `coder-hdd-volume=true` 的 PVC,目录取注解 `coder.com/hdd-volume-path`,按申请值设配额,并清理卷内 `.uploads` 下超过 24 小时的残留分片。单位按二进制换算(Gi = 1024^3)。

```bash
sudo /usr/local/sbin/coder-workspace-quota.py
sudo /usr/local/sbin/coder-hdd-volume-quota.py
sudo xfs_quota -x -c 'report -p -h' /mnt/ssd-data | tail -5
sudo xfs_quota -x -c 'report -p -h' /mnt/hdd-data | tail -5
kubectl -n coder-workspaces exec <pod> -c dev -- df -h /home/coder
```

initContainer 首次把镜像内容拷进卷会占用用户配额,CUDA 版首次拷贝约 6 到 8G,申请值要比预期可用空间留出这些余量。

## 十一、步骤八:实时容量服务与数据卷 API

服务源码在 `deploy/cluster-capacity/`,Go + client-go,只读聚合 k8s 状态,监听 3999。

```bash
cd "$REPO/deploy/cluster-capacity"
export PATH=/usr/local/go/bin:$PATH
export GOPATH=/mnt/ssd-data/mingjia-caches/go GOCACHE=/mnt/ssd-data/mingjia-caches/go-build
export GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=local TMPDIR=/mnt/ssd-data/tmp
go build -o cluster-capacity .
sudo install -m 0755 cluster-capacity /usr/local/bin/cluster-capacity

cd "$REPO"
sudo deploy/install.sh
sudo systemctl enable --now cluster-capacity
```

单元里的启动参数:

| 参数 | 值 | 说明 |
| --- | --- | --- |
| `-addr` / `-namespace` | `:3999` / `coder-workspaces` | 监听地址与统计范围 |
| `-kubeconfig` | `/data/mingjia/.kube/config` | 集群凭据 |
| `-cpu-total` / `-cpu-overcommit` / `-cpu-reserved` | `48` / `2` / `4` | CPU 预算为物理核数乘超分系数再扣预留 |
| `-mem-total-gib` / `-mem-reserved-gib` | `377` / `16` | 内存总量与系统预留 |
| `-disk-budget-gb` | `6800` | 工作区磁盘预算 |
| `-volume-root` / `-volume-storage-class` | `/mnt/hdd-data/volumes` / `coder-hdd` | HDD 卷目录与 StorageClass,单元里由 `__VOLUME_ROOT__`、`__VOLUME_STORAGE_CLASS__` 渲染 |
| `-volume-node` | `ubuntu0002` | HDD 卷绑定节点,单元里由 `__NODE_NAME__` 渲染;程序默认空值,留空时自动取集群唯一节点,多节点时报错要求显式指定 |

`-cpu-overcommit` 必须与模板变量 `cpu_overcommit` 一致,否则页面显示与申请校验口径不同。`-volume-node` 显式传入只是为了让单元与集群现状一致,单节点集群留空也能跑。

### 11.1 接口

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/capacity` | CPU、内存、GPU(按型号)、磁盘余量,3 秒缓存 |
| GET | `/hdd` | HDD 总量、已用、空闲、卷数量、`prjquota_active` |
| GET | `/volumes` | 列出全部卷,含 `owner`、`size_gb`、`phase`、`in_use_by` |
| POST | `/volumes` | 建卷,请求体 `{"owner":"<用户名>","name":"<卷名>","size_gb":100}` |
| GET | `/volumes/{pvc}` | 单个卷详情 |
| DELETE | `/volumes/{pvc}` | 删卷,请求体必须带 `{"confirm":"<pvc>"}`;`?purge=true` 连数据目录一起删,默认只删 k8s 对象 |
| POST | `/volumes/{pvc}/uploads?filename=<名>&size=<字节>` | 开分片上传会话,声明大小超过卷内余量直接拒绝 |
| HEAD、GET、PATCH、DELETE | `/volumes/{pvc}/uploads/{id}` | 查偏移(响应头 `X-Upload-Offset`)、按 `offset` 追加(单片上限 64MiB,偏移不符返回 409)、取消上传 |
| POST | `/volumes/{pvc}/uploads/{id}/complete?...` | 校验后原子落盘到卷根目录,`on_conflict` 取 `overwrite` 或 `rename`,缺省时同名文件返回 409 |
| GET | `/` | HDD 卷申请页,含列表、申请与删除 |

响应带 `Access-Control-Allow-Origin: *` 与 `Cache-Control: no-store`,预检请求返回 204。单卷 100 到 1000 GB、每人 1000 GB 的范围与步长定义在卷页面里,调整时改 `site/src/pages/VolumesPage/VolumesPage.tsx` 的常量。

### 11.2 容量口径

占用等于 `coder-workspaces` 命名空间里所有未结束 Pod 的容器 `limits` 之和,再扣掉系统预留;CPU 预算为物理核数乘超分倍数,内存不超分;磁盘已用是该命名空间内非 HDD 卷的 PVC 申请值之和。与模板里 `scripts/check-capacity.sh` 口径一致,所以页面数字就是还能申请多少。

### 11.3 HDD 数据卷

| 项 | 值 |
| --- | --- |
| PVC 与 PV 名 | `hdd-<owner>-<name>` |
| 数据目录 | `/mnt/hdd-data/volumes/<owner>-<name>` |
| StorageClass | `coder-hdd`(`kubernetes.io/no-provisioner`,`WaitForFirstConsumer`,`Retain`) |
| 绑定节点与挂载点 | `kubernetes.io/hostname=ubuntu0002`(卷模板里是 `__NODE_NAME__`),容器内挂到 `/mnt/data` |
| 回收策略 | `Retain`,删卷默认保留数据目录,只有 `purge=true` 才删 |

`deploy/hdd-volumes/` 里是占位符模板(`__VOLUME_STORAGE_CLASS__`、`__VOLUME_ROOT__`、`__NODE_NAME__`),渲染产物在 `deploy/generated/hdd-volumes/`,apply 的是渲染产物。先建 StorageClass 与占位卷(占位卷让模板可以无条件挂载,避免 plan 阶段出现未知值),再建卷,两种建卷方式等价:

```bash
cd "$REPO"
sudo deploy/install.sh --render-only           # 只渲染,不改系统文件
export KUBECONFIG=/data/mingjia/.kube/config
kubectl apply -f deploy/generated/hdd-volumes/storageclass.yaml
kubectl apply -f deploy/generated/hdd-volumes/placeholder.yaml

curl -X POST http://127.0.0.1:3999/volumes -H 'Content-Type: application/json' \
  -d '{"owner":"alice","name":"dataset","size_gb":100}'
kubectl apply -f deploy/generated/hdd-volumes/example-volume.yaml
```

模板参数 `hdd_volume` 填 PVC 名(例如 `hdd-alice-dataset`)。同一个卷允许同时挂到多个工作区,读写冲突由用户自行保证;删除卷要求没有运行中的工作区在用它。

## 十二、步骤九:WebUI 定制

前端改动只涉及 `site/`,不加后端依赖。三个自定义模块:

| 模块 | 作用 |
| --- | --- |
| `site/src/modules/clusterCapacity/clusterCapacity.ts` | 轮询 `http://<当前主机>:3999/capacity`,整页共用一个 10 秒轮询器;`liveMaxFor` 把实时余量换算成各参数自己的单位 |
| `site/src/modules/clusterCapacity/HddVolumeField.tsx` | 数据卷选择器,只列当前用户的卷,服务不可达时退回普通文本输入 |
| `site/src/modules/workspaces/DynamicParameter/parameterUiHints.ts` | 参数 UI 规则表:步进器范围、联动隐藏、不提供的编辑器入口 |
| `site/src/pages/VolumesPage/{VolumesPage.tsx,VolumeUpload.tsx}` 与 `site/src/pages/CreateWorkspacePage/ClusterCapacityBar.tsx` | 数据卷页面(申请、列表、删除、分片上传)与申请页、参数页顶部的实时容量条 |

规则表内容:

```text
numericSteppers   = { home_disk_size: { min: 100, max: 500, step: 100 },
                      gpu_count:      { min: 1,   max: 4,   step: 1   } }
visibilityRules   = { gpu_count: { dependsOn: "gpu_model", hiddenForValues: ["none"] } }
hiddenDisplayApps = ["vscode", "vscode_insiders"]
```

步进器到上下限时不禁用按钮,而是给出"已达上限"提示;隐藏只是界面行为,值仍会提交,未申请 GPU 时模板本来就忽略 `gpu_count`。范围与步长写在前端常量表 `site/src/modules/workspaces/DynamicParameter/parameterUiHints.ts`,调整时以它为准。容量条在申请页与工作区参数页顶部,每 10 秒刷新并可手动刷新,接口不可达时整条隐藏,不影响申请。

重建与生效:

```bash
cd /data/mingjia/Workspaces-VCL
export PATH=/usr/local/go/bin:/mnt/ssd-data/mingjia-caches/go/bin:$PATH
export GOPATH=/mnt/ssd-data/mingjia-caches/go GOCACHE=/mnt/ssd-data/mingjia-caches/go-build
export GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=local TMPDIR=/mnt/ssd-data/tmp
export MAKEFLAGS='OS_ARCHES=linux_amd64'
make site/out/index.html build/coder_linux_amd64
sudo systemctl restart coder-dev
```

前端嵌在 coderd 二进制里,必须先重建站点再重建二进制,只做一步不会生效。重启后浏览器强制刷新一次,service worker 会缓存旧资源。

## 十三、相对 Coder 上游需要删改的内容

改动范围只在 `site/`:新增 6 个文件、修改 28 个文件、删除 646 个文件,Go 后端未改。逐条清单、基线提交与重现步骤见 `deploy/CHANGES-FROM-UPSTREAM.md`,这里只列操作要点:

1. 在全新上游检出上复现:检出基线提交 `95c9dcaa17`,新增 6 个自定义文件,用本仓库同名文件覆盖 28 个修改文件,按清单删除 646 个文件,再从 `site/src/router.tsx` 移除对应的 lazy import 与路由。
2. 删除的目录:`site/src/pages/AgentsPage/`(490 个文件)、`site/src/pages/AISettingsPage/`(134 个)、`site/src/pages/DeploymentSettingsPage/` 下的 AI 治理与外部认证页面、`site/src/pages/UserSettingsPage/ExternalAuthPage/`、桌面版 VS Code 与 devcontainer 相关组件、`AISettingsSidebar` 系列。
3. 保留但不再暴露入口的:`/external-auth/:provider`(工作区 Git 授权回调)与 `ExternalAuthButton`(创建工作区时的 Authorize 按钮)。
4. 导航保留 Workspaces、Templates、Volumes;管理菜单去掉 AI 项;部署设置侧边栏去掉 Licenses 与 External Authentication;用户设置侧边栏去掉 External Authentication。
5. 默认主题改为 `light`(`site/src/theme/index.ts` 的 `DEFAULT_THEME`),用户仍可在"外观"里切换;通知设置去掉 `Chat Events`、`AI Cost Control Events`、`AI Cost Control Admin Events` 三组;`site/src/serviceWorker.ts` 的兜底跳转改为 `/workspaces`。
6. 桌面版 VS Code 入口全部移除,并删除 `VSCodeDesktopButton`、`VSCodeDevContainerButton`、`AgentDevcontainerCard` 三个组件文件。
7. `site/vite.config.mts` 与 `site/scripts/check-compiler.mjs` 里的 React Compiler 目标目录要跟着删掉的页面同步。
8. 前端之外还有三处部署侧配置要一起到位:控制面参数 `--dangerous-allow-path-app-sharing=true`、模板里 `coder_app.code_server` 的 `share = "authenticated"`、控制面参数 `--dangerous-allow-cors-requests=true`。

## 十四、运维手册

### 14.1 systemd 单元与定时器

| 单元 | 类型 | 作用 | 检查命令 |
| --- | --- | --- | --- |
| `coder-dev.service` | 常驻 | 控制面与内嵌 PostgreSQL | `systemctl status coder-dev` |
| `cluster-capacity.service` | 常驻 | 容量与数据卷服务 `:3999` | `systemctl status cluster-capacity` |
| `coder-workspace-quota.timer` | 定时 | 每分钟给工作区卷设配额,`OnBootSec=2min` | `systemctl list-timers coder-workspace-quota.timer` |
| `coder-hdd-volume-quota.timer` | 定时 | 每分钟给 HDD 卷设配额并清残留分片,`OnBootSec=3min` | `systemctl list-timers coder-hdd-volume-quota.timer` |
| `kubelet`、`containerd` | 常驻 | 集群运行时 | `systemctl status kubelet containerd` |

两个 `.service` 都是 `Type=oneshot`,由同名 timer 拉起,手工触发用 `sudo systemctl start coder-workspace-quota.service`。单元与 timer 都在 `deploy/systemd/`,由 `deploy/install.sh` 渲染后装到 `/etc/systemd/system/`。换机器时只改 `deploy/local.env`(用户名、检出目录、访问地址、存储路径、节点名、kubeconfig、容量参数),然后重新运行 `sudo deploy/install.sh`,不要直接改 `/etc/systemd/system/` 下的单元。

### 14.2 日志位置

| 来源 | 位置 |
| --- | --- |
| coderd 应用日志 | `/mnt/ssd-data/logs/coderd.log`,轮转 100M × 4 |
| 单元日志 | `journalctl -u coder-dev`、`journalctl -u cluster-capacity` |
| 配额任务 | `journalctl -u coder-workspace-quota`、`journalctl -u coder-hdd-volume-quota` |
| kubelet 与运行时 | `journalctl -u kubelet`、`journalctl -u containerd` |
| Pod 日志 | `kubectl -n <命名空间> logs <pod>`,落盘 `/var/log/pods`,每容器 10Mi × 5 份 |
| 系统日志 | `journalctl`(上限 500M)、`/var/log/syslog`(100M × 4) |

### 14.3 日常检查

```bash
export KUBECONFIG=/data/mingjia/.kube/config
kubectl get nodes
kubectl get pods -A | grep -vE 'Running|Completed'
kubectl get clusterpolicy
kubectl get node ubuntu0002 -o jsonpath='{.status.allocatable.nvidia\.com/gpu}'; echo
kubectl get node ubuntu0002 -o jsonpath='{.status.conditions[?(@.type=="DiskPressure")].status}'; echo

curl -s http://127.0.0.1:3001/healthz -o /dev/null -w 'coderd %{http_code}\n'
curl -s http://127.0.0.1:3999/capacity | jq '{cpu,memory,gpu,disk}'
curl -s http://127.0.0.1:3999/hdd | jq .

systemctl list-timers 'coder-*' --no-pager
sudo ls /run/nvidia/driver/usr/lib/x86_64-linux-gnu | wc -l
ls /usr/lib/x86_64-linux-gnu | grep -cE '^lib(nvidia|cuda)'
sudo xfs_quota -x -c 'report -p -h' /mnt/ssd-data | tail -5
sudo xfs_quota -x -c 'report -p -h' /mnt/hdd-data | tail -5

df -h / /mnt/ssd-data /mnt/hdd-data
journalctl --disk-usage
```

### 14.4 故障判定

| 现象 | 判定 | 处理 |
| --- | --- | --- |
| 节点 `nvidia.com/gpu` 变成 0 | `kubectl get node ubuntu0002 -o jsonpath='{.status.allocatable.nvidia\.com/gpu}'` | 驱动重装的初始化容器会等所有 GPU 客户端退出;先删掉卡住的 GPU 客户端 Pod,再删驱动 Pod 触发重装 |
| 容器里找不到 `libnvidia-ml.so` | `sudo ls /run/nvidia/driver/usr/lib/x86_64-linux-gnu \| wc -l`,坏掉时是 0 | 驱动根的目录枚举失效。删驱动 DaemonSet 的 Pod 让它重装,2 到 4 分钟后重新验收 |
| 新 Pod 无法调度,提示污点 | `kubectl get node ubuntu0002 -o jsonpath='{.spec.taints}'` | 根分区空闲低于 15% 时 kubelet 会打 `disk-pressure` 污点;清理镜像或把数据移出根分区,手工去污点只能顶一会儿 |
| 工作区一直 starting | `kubectl -n coder-workspaces get pods`、`describe pod <pod>` | 区分调度失败与镜像拉取失败;调度失败看实时容量接口与节点余量 |
| 容器内 `df` 显示整盘容量 | `findmnt -no OPTIONS /mnt/ssd-data \| grep -o prjquota` | 缺 `prjquota` 时配额脚本会跳过;`/etc/fstab` 加上后重启生效 |
| 容量条不显示 | `curl -s http://127.0.0.1:3999/capacity` | 服务未运行或页面所在主机访问不到 3999;`systemctl restart cluster-capacity` |
| 网页 VS Code 打不开 | Pod 是否就绪,工作区内 `curl -s http://127.0.0.1:13337/healthz` | 确认控制面带 `--dangerous-allow-path-app-sharing=true`,模板里 `share = "authenticated"`;已存在的工作区要更新到最新模板版本后停再启 |
| 停止或删除工作区长时间不结束 | Pod 停在 Terminating 却仍在 Running;`sudo cat /proc/<容器 init 的宿主 PID>/attr/current` 看它是否处于容器 AppArmor profile 下 | 先用 `sudo runc --root /run/containerd/runc/k8s.io kill <容器ID> TERM` 确认信号是否被拒;被拒时用同名桩文件 `apparmor_parser -R` 卸载该 profile 并把源文件移出 `/etc/apparmor.d`;应急时 `kubectl delete pod <pod> --force --grace-period=0` |
| 执行 `systemctl daemon-reload` 后容器 GPU 异常 | 驱动 DaemonSet 与容器内 `nvidia-smi` | systemd cgroup 驱动下 `daemon-reload` 会影响运行中的容器,`sudo systemctl restart containerd` 恢复 |
| 3001 端口被占用 | `ss -ltnp \| grep 3001` | 先 `systemctl stop coder-dev`,再确认没有残留的 `develop.sh` 进程 |

### 14.5 备份

| 内容 | 路径 | 方式 |
| --- | --- | --- |
| Coder 数据库 | `/data/mingjia/Workspaces-VCL/.coderv2/postgres` | 先停 `coder-dev`,再整目录打包或 rsync |
| Coder 访问配置 | `/data/mingjia/Workspaces-VCL/.coderv2/{url,session}` | 文本备份 |
| 集群凭据与证书 | `/etc/kubernetes/pki`、`/etc/kubernetes/admin.conf`、`/data/mingjia/.kube/config` | root 打包 |
| etcd 数据 | `/var/lib/etcd` | etcd 快照,或停 kubelet 后打包 |
| 集群配置 | `/etc/containerd`、`/etc/cni/net.d`、`/etc/default/kubelet`、`/etc/k8s-resolv.conf`、`/etc/sysctl.d/k8s.conf`、`/etc/modules-load.d/k8s.conf`、`/etc/fstab` | root 打包 |
| 工作区持久卷 | `/mnt/ssd-data/local-path` | 停掉工作区后 rsync |
| HDD 数据卷 | `/mnt/hdd-data/volumes` | 停掉挂载它的工作区后 rsync;该卷跨 4 块盘且无冗余,这里不是备份的替代品 |
| 模板、镜像定义、服务与脚本 | 本仓库 `deploy/` | git 提交;`gpu-operator-values.yaml` 可用于重建集群侧组件 |
| 本机取值 | `deploy/local.env` | 被 git 忽略,单独文本备份;换机器时改它后重新运行 `sudo deploy/install.sh` |

### 14.6 重启后检查清单

```bash
export KUBECONFIG=/data/mingjia/.kube/config
kubectl -n gpu-operator get pods                                   # 1 等驱动重装完成,2 到 4 分钟
kubectl get node ubuntu0002 -o jsonpath='{.status.allocatable.nvidia\.com/gpu}'; echo   # 2 返回 10
sudo ls /run/nvidia/driver/usr/lib/x86_64-linux-gnu | wc -l        # 3 数百条
systemctl is-active coder-dev cluster-capacity kubelet containerd  # 4 全部 active,配额定时器 enabled
findmnt -no OPTIONS /mnt/ssd-data | grep -o prjquota               # 5 HDD 同理
curl -s -o /dev/null -w '%{http_code}\n' http://10.129.164.15:3001/healthz   # 6 返回 200
df -h / /mnt/ssd-data /mnt/hdd-data                                # 7 根分区空闲大于 6G
```

工作区若仍显示旧状态,`kubectl -n coder-workspaces get pods` 里残留的 Pod 可以手工删掉,Coder 会在下次启动时重建。

### 14.7 反向代理(nginx)

对外入口是宿主机上的 nginx(发行版包,`systemd` 托管),按域名把请求分流到两个后端:

| 对外 | 后端 | 说明 |
| --- | --- | --- |
| `http://<域名>/`(80) | `127.0.0.1:3001` | 控制面:Web UI、API、工作区代理(Web 端 VS Code、终端、端口转发) |
| `http://<域名>:3999/` | `127.0.0.1:3998` | 容量与数据卷服务:实时余量、卷管理、分片上传 |

选择宿主机二进制而不是容器的原因:入口代理依赖最少(不需要容器运行时即可启动),由 `systemd` 直接托管与随机器启动,升级走发行版包管理,日志与轮转沿用系统配置;容器方案多一层运行时依赖,对纯转发没有收益。

配置放在 `deploy/nginx/workspaces.conf`,由 `deploy/install.sh` 渲染(占位符取自 `deploy/local.env` 的 `DOMAIN`、`HTTP_PORT`、`CAPACITY_PORT`、`HTTP_ADDR`、`CAPACITY_BIND`)并安装到 `/etc/nginx/conf.d/`,同时停用发行版自带的默认站点,避免抢占 80 端口。改域名或端口只需改 `local.env` 后重新执行 `sudo deploy/install.sh`。

代理参数针对本平台的流量特征设置,逐项原因如下:

| 配置 | 作用 |
| --- | --- |
| `proxy_http_version 1.1` + `Upgrade`/`Connection` 映射 | 让终端、Web 端 VS Code、端口转发这类 WebSocket 隧道正常建立 |
| `proxy_request_buffering off`、`client_max_body_size 0`、`client_body_timeout 3600s` | 上传直接透传、不落临时文件、不限体积,并在偏慢的网络下也不会被 60 秒默认超时打断 |
| `proxy_buffering off`、`proxy_max_temp_file_size 0` | 日志、文件下载等流式响应即时下发,不写磁盘缓冲 |
| `proxy_read_timeout` / `proxy_send_timeout` / `send_timeout 3600s` | 长连接隧道不会被中途断开 |
| `proxy_socket_keepalive on` 与 `upstream ... keepalive` | 与后端保持长连接,减少握手开销 |
| `tcp_nodelay on` | 交互式终端与隧道的小包立即发送,降低延迟 |

验收方式(部署后建议各做一次):

```bash
# 控制面与容量服务都能通过域名访问
curl -s -o /dev/null -w '%{http_code}\n' http://<域名>/healthz
curl -s http://<域名>:3999/capacity | head -c 120

# WebSocket 隧道:在工作区页面打开 Terminal 或 Web 端 VS Code,能正常输入输出

# 大文件:分片上传一个 GB 级文件,校验服务端偏移与文件哈希(见 11.1 的上传接口)
```

HTTPS 尚未启用:域名解析到内网地址,无法用 Let's Encrypt 的 HTTP 校验签发证书。后续如需 HTTPS,可在 nginx 上加 443 监听,证书用内部 CA 或自签证书,并把 `local.env` 的 `ACCESS_URL` 改为 `https://<域名>` 后重新执行 `install.sh`。

### 14.8 GPU 历史指标

工作区页面的监控区域(CPU Usage / RAM Usage 下面)会显示一块 GPU 历史看板:曲线是 GPU 利用率(左轴),柱状是显存占用(右轴);同一时刻显示一张卡,用下拉切换显卡,切换时曲线做补间动画;时间范围支持「小时(1/2/4/8/12/24,默认 8)」与「天(1/2/3/5/7)」两档。

数据链路:

```text
DCGM Exporter(GPU Operator 自带,:9400)
   │  每分钟一次,经 k8s API 的 Pod 代理读取 /metrics(不额外暴露端口)
   ▼
gpu-metrics 服务(宿主 systemd,127.0.0.1:3997)
   │  按 (工作区, GPU) 落一行
   ▼
PostgreSQL 库 gpu_metrics(表 gpu_samples,保留 7 天,每小时清理一次)
   ▲
   │  nginx 把 /gpu-api/ 转发到该服务,浏览器同源访问
前端:工作区页面 → AgentGpuHistory 组件
```

要点与运维:

| 项 | 说明 |
| --- | --- |
| 采集范围 | 只记录**申请了 GPU 的工作区**:DCGM 输出里带 `pod` 标签的样本才会入库,未分配的卡不产生数据 |
| 归属方式 | 用 Pod 上的 `com.coder.workspace.id` / `com.coder.workspace.name` / `com.coder.user.username` 标签解析(模板已内置),解析结果带缓存 |
| 采样与保留 | `-interval 1m`、`-retention 168h`(单元文件里可改);数据量约每卡每天 1440 行,整机 10 卡 7 天约 20 MB |
| 数据库 | 复用控制面自带 PostgreSQL,但使用独立库 `gpu_metrics` 与独立角色,不动 Coder 自己的库表;口令写在 `/etc/gpu-metrics.env`(0600),由 `deploy/local.env` 的 `METRICS_PG_URL` 渲染 |
| 接口 | `GET /gpu-api/gpus?workspace_id=<id>` 列出该工作区的卡;`GET /gpu-api/series?workspace_id=<id>&gpu=<uuid>&hours=<n>` 取序列(自动降采样到 600 点以内);`GET /gpu-api/healthz` 查看最近一次采样时间 |
| 可见性 | 指标对所有登录用户可见;工作区内的 VS Code、Terminal 等入口仍按 Coder 自身的权限模型控制,不受此影响 |
| 重建与重启 | `cd deploy/gpu-metrics && go build -o /usr/local/bin/gpu-metrics .` 后 `systemctl restart gpu-metrics`;`sudo deploy/install.sh` 会在能找到 Go 工具链时自动重建 |

首次部署时需要先建库与角色(口令自己生成,只写进 `deploy/local.env`):

```bash
ADMIN=$(./build/coder_linux_amd64 --global-config ./.coderv2 server postgres-builtin-url | sed 's/^psql "//; s/"$//')
# 用上面这个连接串执行:
#   create role gpu_metrics login password '<口令>';
#   create database gpu_metrics owner gpu_metrics;
# 然后把连接串写进 deploy/local.env:
#   METRICS_PG_URL=postgres://gpu_metrics:<口令>@127.0.0.1:41231/gpu_metrics?sslmode=disable
sudo deploy/install.sh && sudo systemctl enable --now gpu-metrics
```

排查:

```bash
systemctl status gpu-metrics                 # 服务状态
journalctl -u gpu-metrics -n 30             # 采集日志(每轮会打印记录的卡数)
curl -s http://127.0.0.1:3997/healthz       # 最近一次成功采样时间与最近一次错误
curl -s http://workspace.mingjia.tech/gpu-api/healthz
```

如果看板上没有数据:确认该工作区申请了 GPU;确认 `kubectl -n gpu-operator get pod -l app=nvidia-dcgm-exporter` 有 Running 的 exporter;再看 `journalctl` 里是否打印"已记录 N 张卡的样本"。

### 14.9 只读实例看板(Dashboard)

左上角导航新增 **Dashboard**:所有登录用户都能看到**当前正在运行的实例**及其配置与用量,但**没有任何操作入口**,也不提供 VS Code / 终端(那些仍然只有工作区属主能用,见 14.10)。

| 列 | 来源 |
| --- | --- |
| 实例 / 属主 | Pod 标签 `com.coder.workspace.name` 与 `com.coder.user.username`:实例名为主行,属主为副行 |
| 状态 | Pod `status.phase`(只列 Running) |
| 配置(CPU/内存/GPU/磁盘) | 容器 `dev` 的 limits + 挂到 `/home/coder` 的 PVC 申请容量 |
| 当前用量(CPU/内存/磁盘) | kubelet Summary API(`/api/v1/nodes/<node>/proxy/stats/summary`,经 k8s API 代理,不需要 metrics-server);磁盘只统计 home 卷 |
| GPU(利用率/显存) | `gpu_samples` 里该工作区最近一次采样 |
| 运行时间 | Pod `status.startTime` |
| GPU 历史按钮 | 打开 14.8 的 GPU 历史曲线(利用率折线 + 显存柱状,可切显卡与小时/天范围) |

实现与权限:

- 接口是 `gpu-metrics` 服务的 `GET /gpu-api/dashboard`(每 15 秒由页面轮询一次)。服务会拿**调用方自己的会话**去控制面 `/api/v2/users/me` 校验,未登录直接 401;不需要任何管理员令牌,也不放宽 Coder 自身的权限模型。
- 前端页面为 `site/src/pages/DashboardPage/DashboardPage.tsx`,只渲染信息与「刷新」「GPU 历史」两个只读操作。

### 14.10 工作区可见性与实例入口的权限边界

| 事项 | 现状 |
| --- | --- |
| 谁能操作工作区(改配置、改 schedule、删除、重启) | 只有属主与管理员;其他人没有权限,API 层直接拒绝 |
| 谁能进实例(Web 端 VS Code、网页终端、SSH) | **只有工作区属主**。路径型 app 被服务端强制成 `owner`,与模板写法无关;终端/pty 端点对非属主返回 404 |
| 谁能看到别人的工作区 | Coder 自身的模型**不支持**"只读可见":工作区默认私有,共享只有 `use` 与 `admin` 两种角色,内置的 Organization Auditor 也不含工作区,自定义角色是企业功能(`custom_roles` 未授权)。因此只读可见性统一走 14.9 的 Dashboard,而不是放宽 Coder 的 ACL |
| 谁能看性能指标 | GPU 历史对所有登录用户可见(14.8);Dashboard 上的 CPU/内存/磁盘是当前值快照 |

注意:模板里 `coder_app` 的 `share` 已改回 `owner`;控制面也移除了 `--dangerous-allow-path-app-sharing` 与 `--dangerous-allow-cors-requests`,因此"路径型 app 只能属主访问"是服务端强制的,老模板建出来的工作区同样生效。

## 十五、附录

### 15.1 关键配置文件

`/etc/modules-load.d/k8s.conf`、`/etc/sysctl.d/k8s.conf`、`/etc/k8s-resolv.conf`、`/etc/default/kubelet` 的完整内容见第五章,`/etc/fstab` 的完整内容见第四章,均已按可直接复制的形式给出。containerd 主配置中必须正确的几项:

```toml
imports = ["/etc/containerd/conf.d/*.toml"]
root = "/mnt/ssd-data/containerd"
state = "/run/containerd"

[plugins."io.containerd.cri.v1.images".pinned_images]
  sandbox = "registry.aliyuncs.com/google_containers/pause:3.10.2"
```

`deploy/gpu-operator-values.yaml`:

```yaml
driver:
  enabled: true
  kernelModuleType: "open"
  version: "595.91.07"
  upgradePolicy:
    autoUpgrade: false

toolkit:
  enabled: true

node-feature-discovery:
  image:
    repository: k8s.m.daocloud.io/nfd/node-feature-discovery
    tag: v0.19.0

cdi:
  enabled: true
```

`deploy/kube-flannel-patched.yml` 相对上游只有一处差异,`flanneld` 的参数多一项:

```yaml
        args:
        - --ip-masq
        - --kube-subnet-mgr
        - --iface=eno1
```

### 15.2 模板参数取值表

见 9.2,那里给出每个参数的 order、类型、默认值、界面取值、是否可变以及落到 Pod 上的效果。

### 15.3 容量服务字段

| 接口 | 字段 |
| --- | --- |
| `/capacity` | `cpu.total`、`cpu.overcommit`、`cpu.budget`、`cpu.reserved`、`cpu.used`、`cpu.free`、`memory.total_gib`、`memory.reserved_gib`、`memory.used_gib`、`memory.free_gib`、`gpu.total`、`gpu.used`、`gpu.free`、`gpu.models.<型号>.{total,memory_gb,used,free}`、`disk.budget_gb`、`disk.used_gb`、`disk.free_gb` |
| `/hdd` 与 `/volumes` | `/hdd` 给出 `total_gb`、`free_gb`、`used_gb`、`volumes`、`prjquota_active`;`/volumes` 的 `volumes[]` 每项含 `name`、`owner`、`pvc`、`path`、`size_gb`、`phase`、`in_use_by` |

### 15.4 目录与路径速查

| 路径 | 内容 |
| --- | --- |
| `/data/mingjia/Workspaces-VCL` | Coder 源码与构建产物;`.coderv2` 在仓库根下,存数据库与会话 |
| `/data/mingjia/Workspaces-VCL/deploy/generated` | `deploy/install.sh` 渲染出的单元、脚本、logrotate 与卷 YAML |
| `/mnt/ssd-data/containerd` | containerd root,镜像与快照 |
| `/mnt/ssd-data/local-path` | 工作区持久卷 |
| `/mnt/ssd-data/logs`、`/mnt/ssd-data/tmp`、`/mnt/ssd-data/mingjia-caches` | coderd 日志、构建临时目录与 Go 缓存 |
| `/mnt/hdd-data/volumes` | HDD 数据卷目录 |
| `/run/nvidia/driver` | 容器化驱动的驱动根,tmpfs,重启后重装 |
| `/usr/local/nvidia/toolkit` | GPU Operator 提供的容器工具链 |

### 15.5 本机不提供的功能

| 项 | 说明 |
| --- | --- |
| NetworkPolicy | Flannel 不执行,工作区之间以及工作区到控制面都互通;需要网络边界要换 Calico 或 Cilium |
| 多节点、HA 与跨机训练 | 单控制平面,HDD 卷绑定 `ubuntu0002`;网卡是 1GbE,无 IB 与 RoCE,正确用法是单机 10 卡 |
| 同一数据卷的多写保护 | 同一个卷允许挂到多个工作区,冲突由用户保证 |
| 桌面版 VS Code 与 Jupyter | 桌面版入口已全部移除,统一用浏览器版 code-server;未引入 Jupyter |
