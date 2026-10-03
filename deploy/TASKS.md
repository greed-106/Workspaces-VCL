# 功能清单与决策记录

本文件是部署的功能账本,只记录三类内容:已经实现并验证的功能、明确的产品与架构决策、仍未完成的待办。
最后核对:2026-10-03,对照运行中的服务、systemd 单元、模板与站点代码。

## 一、已实现并验证的功能

### 控制面

- coderd 由 `coder-dev.service` 托管(User=mingjia、Restart=always、After=kubelet、RequiresMountsFor=/mnt/ssd-data,内嵌 PostgreSQL 自拉起),监听 3001(访问地址 http://10.129.164.15:3001),Prometheus 监听 2114;单元由 deploy/install.sh 按 deploy/local.env 渲染后装到 /etc/systemd/system/。
- coderd 源码与二进制在 /data/mingjia/Workspaces-VCL,这是唯一工作副本,控制面从这里构建与运行;用 `./scripts/develop.sh` 调试前先 `systemctl stop coder-dev`,二者抢 3001 端口。
- 部署配置里的机器相关取值集中在 deploy/local.env(由 deploy/local.env.example 复制,已被 git 忽略);deploy/systemd/、deploy/scripts/、deploy/logrotate/、deploy/hdd-volumes/ 里的模板只写 `__占位符__`(与 local.env 的键同名),`sudo deploy/install.sh` 渲染到 deploy/generated/ 并安装 systemd 单元、配额脚本与 logrotate(`--render-only` 只渲染),渲染出的卷 YAML 在 deploy/generated/hdd-volumes/,供 `kubectl apply`。
- coderd 日志写 /mnt/ssd-data/logs/coderd.log,由 /etc/logrotate.d/coder-dev 轮转(100M × 4、压缩)。
- 网页版 VS Code 对同部署的已登录用户开放:模板 `coder_app` 为 `share = "authenticated"`,coderd 以 `--dangerous-allow-path-app-sharing=true` 启动(非属主打开由 404 变为 302)。
- 集群网络使用 Flannel(未部署 Calico),未启用 NetworkPolicy,工作区之间可以互通。

### 实时容量(cluster-capacity)

- `cluster-capacity`(Go + client-go,实现见 cluster-capacity/main.go)由 `cluster-capacity.service` 常驻,监听 3999。
- `-volume-node` 默认空:为空时服务自动探测,集群只有一个节点就用它,多节点时报错要求显式指定;本机单元仍显式传 `-volume-node __NODE_NAME__`(渲染为 ubuntu0002)。
- `GET /capacity` 返回 CPU、内存、GPU、磁盘的实时空闲,口径与模板的容量检查一致(3 秒缓存),申请页前端每 10 秒轮询一次。
- 服务启动参数就是容量口径:48 核 × 2 倍超分,减去预留 4 核;内存 377 GiB 减去预留 16 GiB;磁盘预算 6800 GB;命名空间 coder-workspaces。
- 申请页与工作区设置页共用实时容量条 `ClusterCapacityBar`(site/src/pages/CreateWorkspacePage/ClusterCapacityBar.tsx),并带手动刷新按钮。
- 加减按钮的实时上限:CPU 8/16/32、内存 16/32/48 按实时空闲过滤,GPU 张数 1-4,磁盘 100-500(步长 100);越界只给提示,不禁用按钮。
- 申请前的容量把关由模板 `terraform_data.capacity_check` 调 coder-template-kubernetes/scripts/check-capacity.sh 完成:几秒内校验 CPU、内存、磁盘、GPU,拒绝时说明原因且不创建任何资源。

### 工作区模板与磁盘

- 模板 `coder-template-kubernetes/main.tf` 的参数顺序:CPU → 内存 → GPU 型号 → GPU 张数 → 开发镜像 → 磁盘大小 → HDD 冷数据卷,不带参数图标。
- GPU 型号由 GPU Operator 的 GFD 节点标签自动探测并生成选项(ubuntu0002 为 NVIDIA-GeForce-RTX-3080-Ti,10 张、单卡 12 GB 显存),选择型号时用 nodeSelector 定位节点;选“不使用显卡”则不申请 GPU,并隐藏张数项。
- 磁盘 project quota:/mnt/ssd-data 与 /mnt/hdd-data 都以 `prjquota` 挂载(已写入 fstab);`coder-workspace-quota.timer`(启动后 2 分钟起、每 1 分钟)运行 /usr/local/sbin/coder-workspace-quota.py,按 PVC 申请值给 local-path 卷设 XFS 硬配额,容器内 `df` 即申请值(实测每个工作区卷 100G 硬限,写超被内核拦下)。
- 工作区持久化目录 persistent_paths = home/coder、usr、etc、opt、var/lib、var/cache,同一个 home PVC 以 subPath 挂到这些路径,stop/start 保留,删除工作区才清空。
- 模板参数与 app 的 share 随模板版本生效:已有工作区需要更新到最新模板版本后 stop/start,才会拿到新的参数界面。

### HDD 冷数据卷

- 卷在工作区之外创建和持有:目录 `/mnt/hdd-data/volumes/<owner>-<卷名>`,PVC 名 `hdd-<owner>-<卷名>`(命名空间 coder-workspaces、storageclass coder-hdd、绑定节点 ubuntu0002),删除工作区不影响卷与数据。
- 卷接口由 cluster-capacity 提供:`GET/POST /volumes`、`GET /hdd`(容量与 `prjquota_active`)、`DELETE /volumes/{pvc}`(需要请求体 `confirm` 等于 PVC 名)、`GET /` 卷申请页。
- 卷申请页的限制只在前端:单卷 100-1000 GB、步长 100、每人合计 1000 GB;实测 API 可以创建 1500 GB 的卷,说明服务端不受限。
- 卷配额由 `coder-hdd-volume-quota.timer`(启动后 3 分钟起、每 1 分钟)运行 /usr/local/sbin/coder-hdd-volume-quota.py,按 PVC 申请值设硬配额(实测 100G);配额是整卷一份,所有挂载它的工作区共用。
- 模板无条件把参数指定的卷挂到 `/mnt/data`,参数留空时挂 1 GiB 占位卷 `hdd-none`;参数值就是 PVC 名。
- 卷文件上传:分片 8 MiB、断点续传、完成时原子落盘;接口 `POST/HEAD/PATCH/DELETE /volumes/{pvc}/uploads` 与 `POST /volumes/{pvc}/uploads/{id}/complete`,界面在 site/src/pages/VolumesPage/VolumeUpload.tsx。
- 卷申请页(`:3999/`)的说明文案与实现一致:容器内路径为 `/mnt/data`。中断上传的残留分片按 24 小时清理,由服务每小时扫描各卷的 `.uploads/`,配额脚本每分钟也会顺带清理。
- 工作区申请页的卷选择器 `HddVolumeField`(site/src/modules/clusterCapacity/HddVolumeField.tsx)只列出当前用户自己的卷(服务不可达时回退为文本输入);`/capacity` 不含 HDD,HDD 容量只在卷申请页显示。

### 镜像与 WebUI

- 两个开发镜像共用 workspace-image/Dockerfile(用 `--build-arg BASE_IMAGE` 切换基础镜像):`coder-workspace:ubuntu24.04-code-server-4.139.1`(默认)与 `coder-workspace:ubuntu24.04-cuda12.8-code-server-4.139.1`(基于官方 nvidia/cuda:12.8.1-devel-ubuntu24.04);统一 coder(uid/gid 1000)身份、sudo 免密。
- 镜像内置 code-server 4.139.1(VS Code 1.139.1),只监听 127.0.0.1:13337,经 Coder 隧道从工作区页面 Apps → VS Code 打开;不预装插件。
- 实测 CUDA 镜像在带 GPU 的 Pod 里 `nvidia-smi` 可见 RTX 3080 Ti,`nvcc` 为 CUDA 12.8。
- WebUI 精简已落地:去掉 GitHub 登录入口、商业版内容(试用 CTA 与 Licenses)、External Authentication 与 AI 设置页(含 AI 通知分组、管理菜单 AI 项)、Agents 页面与 `/agents` 路由、桌面版 VS Code 入口、用户下拉外链。
- 精简后的导航为 Workspaces / Templates / Volumes,管理菜单为 Deployment / Healthcheck;默认主题 light(见 site/src/theme/index.ts 的 DEFAULT_THEME),用户仍可在“外观”里切换。
- 工作区页面与工作区列表不再显示桌面版 VS Code 入口(site/src/modules/workspaces/DynamicParameter/parameterUiHints.ts 的 hiddenDisplayApps 隐藏 vscode 与 vscode_insiders)。

- 对外入口由宿主机 nginx 承担(`deploy/nginx/workspaces.conf` 模板 + `deploy/install.sh` 安装):80 端口转发到控制面(127.0.0.1:3001),3999 端口转发到容量与数据卷服务(127.0.0.1:3998);两个服务都只监听回环,不再直接对外。域名 `workspace.mingjia.tech` 解析到本机内网地址,暂用 HTTP。
- 代理参数按平台流量特征设置:WebSocket 隧道(终端、Web 端 VS Code、端口转发)直通,上传不做请求体缓冲、不限体积、超时放宽到 1 小时,响应不做磁盘缓冲,后端保持长连接。实测 512MiB 与 2GiB 分片上传通过校验(哈希一致),终端隧道内执行命令可正常回显。
- 原先占用 80 端口的 Nextcloud(snap 安装且无用户数据)已移除,移除时 snapd 自动保存了数据快照。

- GPU 历史指标(2026-10-03):新增 `gpu-metrics` 服务(宿主 systemd,127.0.0.1:3997),每分钟经 k8s Pod 代理读取 GPU Operator 自带的 DCGM Exporter,写入控制面自带 PostgreSQL 的独立库 `gpu_metrics`,保留 7 天;前端在工作区监控区域(CPU/RAM 下方)显示「GPU 使用历史」看板:利用率折线 + 显存柱状(双轴)、同一时刻一张卡、小时/天两档范围(默认 8 小时)、切换显卡带补间动画;指标所有登录用户可见,工作区入口仍按 Coder 权限模型。数据量约每卡每天 1440 行,整机 10 卡 7 天约 20 MB。

- 只读实例看板(2026-10-03):左上角新增 `Dashboard` 导航,所有登录用户可见当前正在运行的实例(用户/实例、状态、CPU/内存/GPU/磁盘配置、当前用量、运行时间)与「GPU 历史」按钮(复用 14.8 的曲线,弹窗展示);接口 `GET /gpu-api/dashboard` 用调用方会话向控制面校验身份,未登录 401;页面只有只读操作。权限边界:Coder 原生不支持"跨用户只读可见"(共享角色只有 use/admin,自定义角色未授权),因此只读可见性走自建看板,不放宽 Coder ACL;实例入口(Web 端 VS Code、终端)只有属主可用,已由服务端强制(path app 强制 owner + pty 对非属主 404)。

- GPU 历史接口在服务端做降采样(2026-10-03):按 `ceil(原始点数/600)` 分钟聚合后返回,最多 600 点;实测 7 天(10,080 行)返回 594 点约 62 KB,不会把每分钟的原始数据全传给前端。Dashboard 表格也已去掉 GPU 列(避免与「GPU 历史」按钮重复),实例与用户按「用户 / 实例」展示,弹窗标题同格式,均不再出现「属主」字样。

- 修复「申请数据卷全部失败」(2026-10-03):根因是控制面 CSP 只允许 `connect-src 'self'`,而切到域名访问后前端仍在请求 `http://<域名>:3999`(跨端口即跨源),浏览器直接拦下,请求根本没发出。修法:nginx 增加同源路径 `/capacity-api/` 转发到容量服务(127.0.0.1:3998),前端统一用 `capacityUrl()`,不再拼 hostname + 端口;实测浏览器里列表(200)、申请卷(界面显示「数据目录已建好」)、分片上传(offset 8388608、complete 200)全部正常。

- 数据卷用量与看板(2026-10-03):Volumes 页面顶部改为 hdd-data 实时看板(总计/已用/空闲/卷数 + 使用率条 + 手动刷新,10 秒轮询);新增「全部数据卷」只读表(属主/卷名/申请大小/实际使用/使用率/状态/使用中),「我的数据卷」也补了使用量与使用率。用量由配额定时器(root)读 `xfs_quota report -p -b -n` 后写入 `/run/coder-hdd-usage.json`(0644 原子写),cluster-capacity 服务以普通用户读该快照,快照缺失时 `used_gb=null`、页面显示「—」。
- 数据卷扩容/缩容结论(2026-10-03 实测):改 PVC 被 k8s 拒绝(静态 PV 不允许 resize);容量实际是 XFS project quota 的 bhard,可直接调大或调小,数据不动;XFS 允许把上限设到低于已用(实测 Used 200 MiB / Hard 100 MiB,退出码 0),后果是后续写入被拒,故接口应拒绝「缩到小于已用」。落地建议:注解 `coder.com/hdd-volume-size-gb` 作为权威上限 + 配额脚本优先读取 + 服务 `PATCH /volumes/{pvc}` 与页面入口(尚未实现)。

- 数据卷扩容/缩容:决定**不做**该功能(2026-10-03),仅把实测结论留在运维手册 14.12(改 PVC 被 k8s 拒绝、XFS 可调大调小、允许把上限设到低于已用)与复现步骤;若将来要做,方案为注解 `coder.com/hdd-volume-size-gb` + 配额脚本优先读取 + `PATCH /volumes/{pvc}`。
- 测试数据卷清理(2026-10-03):删除 `hdd-admin-hddtest`(purge 因目录里有工作区映射用户创建的文件而失败,改用 sudo 清理)、孤儿目录 `xcjiang-数据集` 与此前 6 个测试目录;现仅保留占位卷 `hdd-none` 与 `xcjiang/dataset`。运维手册 14.11 记录了 purge 的权限注意点。

## 二、产品与架构决策

- 一个 HDD 数据卷允许同时挂载到多个工作区:读写冲突由用户自行保证,不做互斥拦截;页面上的“使用中”只是提示信息。
- 不做回收站:卷删除只做二次确认(confirm 等于 PVC 名),数据目录按 Retain 保留;以后需要恢复机制再单独做。
- CPU 按 2 倍超分(48 核对应 96 核可分配预算),内存严格不超分,避免 OOM;GPU 与磁盘不超分。
- 工作区镜像只保留 Ubuntu-24.04 与 Ubuntu-24.04(CUDA 12.8),默认基础版。
- 不预装 VS Code 插件:用户在浏览器里自行安装,插件与设置随 /home/coder 持久化。
- 不引入 Jupyter 或 JupyterLab。
- 不支持桌面版 VS Code:只提供浏览器版 code-server,入口在工作区页面 Apps。
- 去掉 AI 与 Agents 相关界面(Agents 页面、AI 设置页、AI 通知分组、管理菜单 AI 项);coderd/x/chatd 后端与 site/src/api/queries/chats.ts 等 API 包装层保留,它们不影响界面。
- 不保留 External Authentication 设置入口,但保留 `/external-auth/:provider`(工作区 Git 授权回调)与 `ExternalAuthButton`(创建工作区时的 Authorize 按钮)。
- 不做模板定时自动推送:容量与参数上限改为前端实时从 cluster-capacity 拉取。
- GPU 张数在模板里是自由数字(CLI 可以申请 3、5、8 张),只有界面按钮限制为 1-4;真实卡数由申请前容量检查与调度器把关。
- 磁盘 100-500 GB、数据卷单卷 100-1000 GB 与每人 1000 GB 都只是前端限制:模板不写 validation、服务端不校验,CLI/API 不受限。
- 工作区持久化只覆盖 persistent_paths 列出的目录,/tmp、/var/run、/root 不持久化。
- 集群网络沿用 Flannel,不引入 Calico,也不依赖 NetworkPolicy 做工作区隔离。
- HDD 容量不计入工作区申请页的容量条,只在卷申请页显示。
- 部署配置模板化:机器相关取值只写在 deploy/local.env(已被 git 忽略),仓库里的单元、脚本、logrotate 与卷 manifest 只写 `__占位符__`,由 deploy/install.sh 渲染与安装;换机器改 local.env 后重跑 `sudo deploy/install.sh`,不直接改 /etc/systemd/system/ 下的单元。

## 三、仍未完成的待办

- 决定是否把 mjyang 从 owner 降为 member(现状:admin 与 mjyang 是部署 owner,xcjiang 是 member;member 只能给自己建工作区)。
- 决定是否隐藏 OAuth2 Applications(`/deployment/oauth2-provider/apps`);它与 GitHub 无关,目前保留。
- 决定是否删除 AI Gateway / AI sessions 界面(`/ai-gateway/sessions`、`/aibridge*` 跳转、site/src/pages/AIBridgePage/、site/src/api/queries/aiBridge.ts);它需要商业授权 aibridge,菜单里不会出现,只能手输 URL 进入。
- 按需给 CUDA 镜像补 python3:在 workspace-image/Dockerfile 增加 python3 python3-pip python3-venv 后 `./build.sh cuda` 重建(或让用户在工作区 `sudo apt install`)。
