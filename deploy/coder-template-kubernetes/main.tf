terraform {
  required_providers {
    coder = {
      source = "coder/coder"
    }
    kubernetes = {
      source = "hashicorp/kubernetes"
    }
  }
}

provider "coder" {
}

variable "use_kubeconfig" {
  type        = bool
  description = <<-EOF
  Coder 控制面是否运行在集群之外?本机 coderd 跑在宿主机上,所以用 true,
  由 provisioner 读取宿主机的 ~/.kube/config。
  EOF
  default     = true
}

variable "namespace" {
  type        = string
  description = "工作区创建在哪个命名空间(需要预先存在)"
}

variable "storage_capacity_gb" {
  type        = number
  description = "可用于工作区持久卷的磁盘总预算(GB)。用于限制磁盘选项上限:已申请的 PVC 会从这里扣减。"
  default     = 6800
}

variable "cpu_overcommit" {
  type        = number
  description = "CPU 超分倍数。CPU 是可压缩资源(工作区大多空闲),所以按 核数 × 该倍数 作为可分配预算;内存不超分。"
  default     = 2
}

variable "system_reserve_cpu" {
  type        = number
  description = "给系统和集群组件预留的 CPU 核数(从可分配总量里扣除)"
  default     = 4
}

variable "system_reserve_mem_gb" {
  type        = number
  description = "给系统和集群组件预留的内存(GB)"
  default     = 16
}

variable "workspace_image" {
  type        = string
  description = "工作区容器镜像(本机可达的镜像源)"
  default     = "docker.1panel.live/codercom/example-base:ubuntu"
}

data "coder_parameter" "cpu" {
  name         = "cpu"
  display_name = "CPU"
  order        = 1
  description  = "分配给这个工作区的 CPU 核数。"
  default      = "8"
  mutable      = true
  dynamic "option" {
    for_each = local.cpu_options
    content {
      name  = "${option.value} Cores"
      value = option.value
    }
  }
}

data "coder_parameter" "memory" {
  name         = "memory"
  display_name = "Memory"
  order        = 2
  description  = "分配给这个工作区的内存(GB)。"
  default      = "16"
  mutable      = true
  dynamic "option" {
    for_each = local.mem_options
    content {
      name  = "${option.value} GB"
      value = option.value
    }
  }
}

data "coder_parameter" "gpu_model" {
  name         = "gpu_model"
  display_name = "GPU 型号"
  order        = 3
  description  = "可选的 GPU 型号,括号内是显存。当前空闲数量见页面顶部的实时提示。选“不使用显卡”则创建纯 CPU 工作区。"
  default      = "none"
  mutable      = true

  option {
    name  = "不使用显卡(纯 CPU)"
    value = "none"
  }

  dynamic "option" {
    for_each = local.gpu_models
    content {
      name  = "${replace(option.key, "NVIDIA-", "")}(${option.value.memory}GB 显存)"
      value = option.key
    }
  }
}

data "coder_parameter" "gpu_count" {
  name         = "gpu_count"
  order        = 4
  display_name = "GPU 张数"
  description  = "在所选 GPU 型号上申请几张卡。型号选“不使用显卡(纯 CPU)”时,这一项会隐藏。"
  default      = "1"
  type         = "number"
  form_type    = "input"
  # 故意用自由数字而不是选项列表:选项会被服务端强制,那样 CLI/API 就没法申请
  # 3、5、8 这类张数了。界面上的加减按钮由前端规则提供,服务端不做限制,
  # 是否真的有这么多卡由申请前的容量检查 + 调度器把关。
  mutable   = true
}

data "coder_parameter" "image" {
  name         = "image"
  display_name = "开发镜像"
  order        = 5
  description  = "工作区使用的容器镜像。需要 GPU 做 CUDA 开发时选带 CUDA 的版本。"
  default      = "coder-workspace:ubuntu24.04-code-server-4.139.1"
  mutable      = false
  # 基础版:Ubuntu 24.04 + 浏览器版 VS Code
  option {
    name  = "Ubuntu-24.04"
    value = "coder-workspace:ubuntu24.04-code-server-4.139.1"
  }
  # CUDA 版:官方 nvidia/cuda:12.8.1-devel-ubuntu24.04 + 浏览器版 VS Code
  option {
    name  = "Ubuntu-24.04(CUDA 12.8)"
    value = "coder-workspace:ubuntu24.04-cuda12.8-code-server-4.139.1"
  }
}

data "coder_parameter" "home_disk_size" {
  name         = "home_disk_size"
  order        = 6
  display_name = "磁盘大小"
  description  = "整个工作区可用的磁盘容量(GB),可选 100 到 500。"
  default      = "100"
  type         = "number"
  form_type    = "input"
  # 界面上的范围与步长写在前端的常量表里,按参数名匹配:
  #   site/src/modules/workspaces/DynamicParameter/DynamicParameter.tsx
  #   -> numericSteppers.home_disk_size
  # 这里不写 validation,是为了让 CLI/API 仍能创建超出该范围的工作区。
  # 调整范围时两边要一起改。
  mutable   = false
}

data "coder_parameter" "hdd_volume" {
  name         = "hdd_volume"
  order        = 7
  display_name = "HDD 冷数据卷"
  description  = "把已有的 HDD 持久卷挂进工作区(容器内路径 /mnt/data)。在「Volumes」页面选择自己的卷,留空表示不挂载;改这一项会重启工作区,卷里的数据不受影响。"
  default      = ""
  type         = "string"
  form_type    = "input"
  # 只保留参数:挂载逻辑待做 —— 参数驱动的 dynamic/count 在 Coder 里会因
  # "值在 plan 阶段未知" 而失败,需改成"始终挂载 + 缺省指向一个占位 PVC"的写法。
  mutable = true
}


provider "kubernetes" {
  config_path = var.use_kubeconfig == true ? "~/.kube/config" : null
}

data "coder_workspace" "me" {}
data "coder_workspace_owner" "me" {}

locals {
  # 需要持久化的路径(相对 PVC 根目录)。stop/start 后这些目录内容保留,
  # 只有删除工作区时才一起删掉。
  # 不放进来的:/tmp(重启后残留的锁文件/套接字容易出怪问题)、/var/run、/root(容器不是 root 身份)。
  persistent_paths = ["home/coder", "usr", "etc", "opt", "var/lib", "var/cache"]
  # HDD 冷数据卷:参数留空时挂占位卷 hdd-none。
  # 注意:这里只能用条件"表达式",不能用 count/for_each —— 参数值在 plan 阶段未知。
  # 参数里填的是卷的 PVC 名(页面上直接可见、可复制),例如 hdd-admin-dataset。
  hdd_claim_name  = trimspace(data.coder_parameter.hdd_volume.value) == "" ? "hdd-none" : trimspace(data.coder_parameter.hdd_volume.value)

  # 主机名:pku-vcl-<工作区 id 的 md5 前 4 位>,工作区创建后固定不变
  hostname = "pku-vcl-${substr(md5(data.coder_workspace.me.id), 0, 4)}"

  # ---------- 容量探测(每次 plan 都实时读取)----------
  # 节点容量
  node_cpu_total = sum([for n in data.kubernetes_nodes.all.nodes :
    try(tonumber(n.status[0].allocatable["cpu"]), 0)])
  node_mem_total_gib = floor(sum([for n in data.kubernetes_nodes.all.nodes :
    try(tonumber(replace(n.status[0].allocatable["memory"], "Ki", "")), 0)]) / 1048576)

  # 工作区已申请的 CPU / 内存(只看 coder-workspaces 命名空间里未结束的 Pod)
  # 所有未结束的 Pod,再排除掉本工作区自己的(重启/更新时不能把自己算成占用)
  live_pods_all = [for p in data.kubernetes_resources.ws_pods.objects :
    p if !contains(["Succeeded", "Failed"], try(p.status.phase, ""))]
  live_pods = [for p in local.live_pods_all :
    p if try(p.metadata.labels["com.coder.workspace.id"], "") != data.coder_workspace.me.id]
  cpu_used = try(sum([for p in local.live_pods :
    try(sum([for c in try(p.spec.containers, []) :
      try(tonumber(try(c.resources.limits["cpu"], null)), 0)]), 0)]), 0)
  mem_used_gib = try(sum([for p in local.live_pods :
    try(sum([for c in try(p.spec.containers, []) :
      try(tonumber(replace(tostring(try(c.resources.limits["memory"], null)), "Gi", "")), 0)]), 0)]), 0)

  # CPU 走超分预算(默认 2 倍):48 核的机器 → 96 核可分配预算,再扣掉系统预留与实际申请。
  cpu_budget = floor(local.node_cpu_total * var.cpu_overcommit)
  free_cpu   = max(local.cpu_budget - local.cpu_used - var.system_reserve_cpu, 0)
  free_mem_gib = max(local.node_mem_total_gib - local.mem_used_gib - var.system_reserve_mem_gb, 0)

  # 磁盘预算:总预算 - 已申请的 PVC 之和
  own_pvc_name = "coder-${data.coder_workspace.me.id}-home"
  pvc_requests_gb = try(sum([for p in data.kubernetes_resources.all_pvcs.objects :
    p.metadata.name == local.own_pvc_name ? 0 :
    try(tonumber(replace(tostring(p.spec.resources.requests.storage), "Gi", "")), 0)]), 0)
  free_disk_gb = max(var.storage_capacity_gb - local.pvc_requests_gb, 0)

  # ---------- GPU 型号探测 ----------
  # 自动探测集群里的 GPU 型号与数量。
  # 数据来自 GPU Operator 的 GFD 写入的节点标签 nvidia.com/gpu.product / .count。
  # 注意:参数选项在模板版本创建(推送)时固定,GPU 型号变化后需要重新推送模板。
  gpu_nodes = [for n in data.kubernetes_nodes.all.nodes : {
    product = try(n.metadata[0].labels["nvidia.com/gpu.product"], "")
    count   = try(tonumber(n.metadata[0].labels["nvidia.com/gpu.count"]), 0)
    memory  = try(tonumber(n.metadata[0].labels["nvidia.com/gpu.memory"]), 0)
  } if try(n.metadata[0].labels["nvidia.com/gpu.product"], "") != ""]

  # 型号 -> { 总数, 显存(GB) }
  gpu_models = { for m in distinct([for n in local.gpu_nodes : n.product]) : m => {
    total  = sum([for n in local.gpu_nodes : n.count if n.product == m])
    memory = floor(max([for n in local.gpu_nodes : n.memory if n.product == m]...) / 1024)
  } }

  # Pod -> 节点 -> 型号,统计每个型号已被申请的卡数(只统计已调度到节点的 Pod)
  node_product = { for n in data.kubernetes_nodes.all.nodes : n.metadata[0].name =>
    try(n.metadata[0].labels["nvidia.com/gpu.product"], "") }
  gpu_pods = [for p in local.live_pods : {
    node = try(p.spec.nodeName, "")
    gpus = try(sum([for c in try(p.spec.containers, []) :
      try(tonumber(try(c.resources.limits["nvidia.com/gpu"], null)), 0)]), 0)
  } if try(sum([for c in try(p.spec.containers, []) :
      try(tonumber(try(c.resources.limits["nvidia.com/gpu"], null)), 0)]), 0) > 0]

  gpu_used_by_model = { for m in distinct([for n in local.gpu_nodes : n.product]) : m =>
    try(sum([for x in local.gpu_pods : x.gpus if try(local.node_product[x.node], "") == m]), 0) }

  # 型号 -> 空闲卡数
  gpu_free_by_model = { for m, info in local.gpu_models :
    m => max(info.total - try(local.gpu_used_by_model[m], 0), 0) }

  # 界面上最大的"可选张数"(取所有型号里最空闲的那个,真正的组合校验在 precondition 里)
  gpu_free_max = try(max([for v in values(local.gpu_free_by_model) : v]...), 0)

  # ---------- 供界面使用的选项(按当前空闲容量过滤)----------
  cpu_options = concat(["8"], local.free_cpu >= 16 ? ["16"] : [], local.free_cpu >= 32 ? ["32"] : [])
  mem_options = concat(["16"], local.free_mem_gib >= 32 ? ["32"] : [], local.free_mem_gib >= 48 ? ["48"] : [])

  # 选了具体型号时,把 Pod 限制到该型号的节点上;选"不使用显卡"则完全不申请
  use_gpu = data.coder_parameter.gpu_model.value != "none"
  gpu_node_selector = local.use_gpu ? {
    "nvidia.com/gpu.product" = data.coder_parameter.gpu_model.value
  } : {}
  gpu_limit = local.use_gpu ? { "nvidia.com/gpu" = data.coder_parameter.gpu_count.value } : {}
}

data "kubernetes_nodes" "all" {}

data "kubernetes_resources" "ws_pods" {
  api_version = "v1"
  kind        = "Pod"
  namespace   = var.namespace
}

data "kubernetes_resources" "all_pvcs" {
  api_version = "v1"
  kind        = "PersistentVolumeClaim"
  namespace   = var.namespace
}

resource "coder_agent" "main" {
  os             = "linux"
  arch           = "amd64"
  startup_script = <<-EOT
    set -e
    # 容器内统一使用镜像自带的 coder 账号(uid 1000),不做改名。
    # 浏览器版 VS Code(code-server)已内置在镜像里(固定版本),
    # 由 coder_script.code_server / coder_app.code_server 启动与暴露。
    echo "workspace ready: $(id -un)@$(hostname) cpus=$(nproc) home=$HOME"
  EOT

  metadata {
    display_name = "CPU Usage"
    key          = "0_cpu_usage"
    script       = "coder stat cpu"
    interval     = 10
    timeout      = 1
  }

  metadata {
    display_name = "RAM Usage"
    key          = "1_ram_usage"
    script       = "coder stat mem"
    interval     = 10
    timeout      = 1
  }

  metadata {
    display_name = "Home Disk"
    key          = "3_home_disk"
    script       = "coder stat disk --path $${HOME}"
    interval     = 60
    timeout      = 1
  }
}

# 申请前先把关:apply 一开始(几秒内)就检查集群是否真有这么多资源。
# 用 Terraform 内置的 terraform_data,不需要额外 provider;
# local-exec 里只做比较与报错,没有副作用。
# 浏览器版 VS Code:镜像里已内置固定版本的 code-server(4.139.1),
# 这里只负责启动并注册成 Coder 的 App 入口。不预装任何插件:
# 用户在浏览器里自行安装,插件与设置都落在持久化的 /home/coder 下。
resource "coder_script" "code_server" {
  agent_id           = coder_agent.main.id
  display_name       = "VS Code"
  icon               = "/icon/code.svg"
  run_on_start       = true
  start_blocks_login = false
  script             = <<-EOT
    #!/usr/bin/env bash
    set -euo pipefail
    if curl -fsS "http://127.0.0.1:13337/healthz" >/dev/null 2>&1; then
      echo "code-server 已在运行"
      exit 0
    fi
    echo "启动 code-server: $(code-server --version | head -1)"
    nohup code-server \
      --bind-addr 127.0.0.1:13337 \
      --auth none \
      --disable-telemetry \
      --disable-update-check \
      /home/coder > "$HOME/.code-server.log" 2>&1 &
    for _ in $(seq 1 30); do
      if curl -fsS "http://127.0.0.1:13337/healthz" >/dev/null 2>&1; then
        echo "code-server 就绪(仅监听 localhost,通过 Coder 代理访问)"
        exit 0
      fi
      sleep 1
    done
    echo "code-server 启动超时,日志尾部:" >&2
    tail -20 "$HOME/.code-server.log" >&2 || true
    exit 1
  EOT
}

# 只监听 localhost,经由 Coder 的加密隧道访问,不开放任何入站端口。
resource "coder_app" "code_server" {
  agent_id     = coder_agent.main.id
  slug         = "code-server"
  display_name = "VS Code"
  icon         = "/icon/code.svg"
  url          = "http://127.0.0.1:13337/"
  subdomain    = false
  # 默认(不写 share)是 owner:只有工作区属主能打开,连管理员/owner 都会拿到 404,
  # 于是出现"网页终端能进、网页 VS Code 进不去"的割裂。这里放开给所有已登录用户,
  # 与终端、以及"成员可以互相建工作区"的现状一致。想收紧改回 "owner" 即可。
  share        = "authenticated"
  open_in      = "slim-window"

  healthcheck {
    url       = "http://127.0.0.1:13337/healthz"
    interval  = 5
    threshold = 6
  }
}

resource "terraform_data" "capacity_check" {
  triggers_replace = [data.coder_workspace.me.start_count]

  provisioner "local-exec" {
    command = "sh scripts/check-capacity.sh"
    environment = {
      START_COUNT = data.coder_workspace.me.start_count
      REQ_CPU     = data.coder_parameter.cpu.value
      FREE_CPU    = local.free_cpu
      REQ_MEM     = data.coder_parameter.memory.value
      FREE_MEM    = local.free_mem_gib
      REQ_DISK    = data.coder_parameter.home_disk_size.value
      FREE_DISK   = local.free_disk_gb
      USE_GPU     = local.use_gpu
      REQ_GPU     = data.coder_parameter.gpu_count.value
      FREE_GPU    = try(local.gpu_free_by_model[data.coder_parameter.gpu_model.value], 0)
      GPU_MODEL   = data.coder_parameter.gpu_model.value
    }
  }
}

resource "kubernetes_persistent_volume_claim_v1" "home" {
  metadata {
    name      = "coder-${data.coder_workspace.me.id}-home"
    namespace = var.namespace
    labels = {
      "app.kubernetes.io/name"     = "coder-pvc"
      "app.kubernetes.io/instance" = "coder-pvc-${data.coder_workspace.me.id}"
      "app.kubernetes.io/part-of"  = "coder"
      //Coder-specific labels.
      "com.coder.resource"       = "true"
      "com.coder.workspace.id"   = data.coder_workspace.me.id
      "com.coder.workspace.name" = data.coder_workspace.me.name
      "com.coder.user.id"        = data.coder_workspace_owner.me.id
      "com.coder.user.username"  = data.coder_workspace_owner.me.name
    }
    annotations = {
      "com.coder.user.email" = data.coder_workspace_owner.me.email
    }
  }
  depends_on       = [terraform_data.capacity_check]
  wait_until_bound = false
  # 注意:这里曾加过 lifecycle/precondition 做"组合资源校验",但 Coder 的模板预览
  # 解析 plan 时遇到未知值会 panic("panic in preview: value is unknown"),所以去掉。
  # 实际防超分靠两层:k8s 调度器(资源不足的 Pod 会 Pending)+ 磁盘 project quota。


  spec {
    access_modes       = ["ReadWriteOnce"]
    storage_class_name = "local-path"
    resources {
      requests = {
        storage = "${data.coder_parameter.home_disk_size.value}Gi"
      }
    }
  }
}

resource "kubernetes_deployment_v1" "main" {
  count = data.coder_workspace.me.start_count
  depends_on = [
    kubernetes_persistent_volume_claim_v1.home,
    terraform_data.capacity_check,
  ]
  # true = terraform 会等 Pod 真正就绪;资源不够(抢不到 GPU/CPU/内存)时构建会失败,
  # Coder 会明确告诉用户"创建失败",而不是给一个永远 Pending 的"成功"工作区。
  wait_for_rollout = true
  timeouts {
    create = "5m"
    update = "5m"
  }
  metadata {
    name      = "coder-${data.coder_workspace.me.id}"
    namespace = var.namespace
    labels = {
      "app.kubernetes.io/name"     = "coder-workspace"
      "app.kubernetes.io/instance" = "coder-workspace-${data.coder_workspace.me.id}"
      "app.kubernetes.io/part-of"  = "coder"
      "com.coder.resource"         = "true"
      "com.coder.workspace.id"     = data.coder_workspace.me.id
      "com.coder.workspace.name"   = data.coder_workspace.me.name
      "com.coder.user.id"          = data.coder_workspace_owner.me.id
      "com.coder.user.username"    = data.coder_workspace_owner.me.name
    }
    annotations = {
      "com.coder.user.email" = data.coder_workspace_owner.me.email
    }
  }

  spec {
    replicas = 1
    selector {
      match_labels = {
        "app.kubernetes.io/name"     = "coder-workspace"
        "app.kubernetes.io/instance" = "coder-workspace-${data.coder_workspace.me.id}"
        "app.kubernetes.io/part-of"  = "coder"
        "com.coder.resource"         = "true"
        "com.coder.workspace.id"     = data.coder_workspace.me.id
        "com.coder.workspace.name"   = data.coder_workspace.me.name
        "com.coder.user.id"          = data.coder_workspace_owner.me.id
        "com.coder.user.username"    = data.coder_workspace_owner.me.name
      }
    }
    strategy {
      type = "Recreate"
    }

    template {
      metadata {
        labels = {
          "app.kubernetes.io/name"     = "coder-workspace"
          "app.kubernetes.io/instance" = "coder-workspace-${data.coder_workspace.me.id}"
          "app.kubernetes.io/part-of"  = "coder"
          "com.coder.resource"         = "true"
          "com.coder.workspace.id"     = data.coder_workspace.me.id
          "com.coder.workspace.name"   = data.coder_workspace.me.name
          "com.coder.user.id"          = data.coder_workspace_owner.me.id
          "com.coder.user.username"    = data.coder_workspace_owner.me.name
        }
      }
      spec {
        # 用 nvidia runtime class + libnvidia-container 注入驱动库。
        # 本机 GPU Operator 的 CDI 规范只挂了 nvidia-smi 二进制、没挂 libcuda/libnvidia-ml,
        # 走 legacy 路径(nvidia-container-runtime)才会把驱动库正确挂进来。
        runtime_class_name = "nvidia"

        # 主机名固定为 pku-vcl-<4 位哈希>,工作区创建后不变
        hostname = local.hostname

        # 用户在界面上选了具体 GPU 型号时,把 Pod 限制到该型号的节点
        node_selector = local.gpu_node_selector

        security_context {
          run_as_user     = 1000
          fs_group        = 1000
          run_as_non_root = true
        }

        # 首次启动时把镜像里的内容拷进空卷,否则空卷会盖住镜像自带的文件。
        # 完成后写标记,后续启停不再重复拷贝。
        init_container {
          name              = "init-persist"
          image             = data.coder_parameter.image.value
          image_pull_policy = "IfNotPresent"
          command = ["sh", "-c", <<-EOT
            set -eu
            mkdir -p /persist/.coder-init
            for p in ${join(" ", local.persistent_paths)}; do
              dst="/persist/$p"
              marker="/persist/.coder-init/$(echo "$p" | tr '/' '_')"
              mkdir -p "$dst"
              if [ ! -f "$marker" ]; then
                echo "init-copy: /$p -> $dst"
                if [ -d "/$p" ]; then cp -a "/$p/." "$dst/" || true; fi
                touch "$marker"
              fi
            done
            echo "init-persist done"
          EOT
          ]
          security_context {
            run_as_user     = "0"
            run_as_non_root = false
          }
          volume_mount {
            name       = "home"
            mount_path = "/persist"
          }
        }

        container {
          name              = "dev"
          image             = data.coder_parameter.image.value
          image_pull_policy = "IfNotPresent"
          command           = ["sh", "-c", coder_agent.main.init_script]
          security_context {
            run_as_user = "1000"
          }
          env {
            name  = "CODER_AGENT_TOKEN"
            value = coder_agent.main.token
          }
          env {
            name  = "HOME"
            value = "/home/coder"
          }
          env {
            name  = "OMP_NUM_THREADS"
            value = data.coder_parameter.cpu.value
          }
          resources {
            requests = {
              "cpu"    = "250m"
              "memory" = "512Mi"
            }
            limits = merge(
              {
                "cpu"    = data.coder_parameter.cpu.value
                "memory" = "${data.coder_parameter.memory.value}Gi"
              },
              local.gpu_limit
            )
          }
          # 持久化挂载:同一个 PVC 的不同子目录,分别挂到对应路径。
          # 这样 stop/start 之后这些目录里的改动都还在,只有删除工作区才清空。
          dynamic "volume_mount" {
            for_each = local.persistent_paths
            content {
              name       = "home"
              mount_path = "/${volume_mount.value}"
              sub_path   = volume_mount.value
              read_only  = false
            }
          }
          volume_mount {
            name       = "tmp"
            mount_path = "/tmp"
            read_only  = false
          }
          volume_mount {
            name       = "tmp"
            mount_path = "/var/tmp"
            read_only  = false
          }
          # HDD 冷数据卷(参数留空时为占位卷)
          volume_mount {
            name       = "hdd"
            # 统一挂到 /mnt/data:卷标识只用于区分,不进路径
            mount_path = "/mnt/data"
            read_only  = false
          }
        }

        # HDD 卷的 PVC 由 cluster-capacity 服务在工作区之外创建(名字 hdd-<owner>-<卷名>),
        # 因此工作区删掉、重建都不影响卷里的数据;参数留空时挂的是 1GiB 占位卷。
        volume {
          name = "hdd"
          persistent_volume_claim {
            claim_name = local.hdd_claim_name
            read_only  = false
          }
        }

        # /tmp 与 /var/tmp 用内存盘:不占节点磁盘,Pod 重建即清空,
        # 容量同时受内存限额约束(超了会 OOM,这是有意为之)。
        volume {
          name = "tmp"
          empty_dir {
            medium     = "Memory"
            size_limit = "8Gi"
          }
        }

        volume {
          name = "home"
          persistent_volume_claim {
            claim_name = kubernetes_persistent_volume_claim_v1.home.metadata.0.name
            read_only  = false
          }
        }
      }
    }
  }
}
