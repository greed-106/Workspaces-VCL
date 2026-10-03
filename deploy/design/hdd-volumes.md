# HDD 持久卷:现状设计说明

本文描述 HDD 持久卷当前的实际实现,不包含规划内容。相关代码:`deploy/cluster-capacity/`(Go 服务)、`deploy/scripts/coder-hdd-volume-quota.py`(配额脚本)、`deploy/hdd-volumes/`(k8s 对象模板)、`deploy/coder-template-kubernetes/main.tf`(模板挂载)、`site/src/pages/VolumesPage/` 与 `site/src/modules/clusterCapacity/`(前端)。

## 一、目标与结论

- 用户可以申请一块大容量 HDD 空间存放冷数据。卷独立于工作区存在:工作区删除、重建都不影响卷里的数据,卷可以反复挂到该用户自己的任意工作区。
- 卷由 cluster-capacity 服务在工作区之外创建并持有(宿主目录 + 静态 PV + PVC),模板只负责挂载。
- 容量硬限制由 XFS project quota 在内核层执行;单卷 100 到 1000 GB、每人合计 1000 GB 的业务限制只在前端做,服务端 API 不限制,命令行可以申请更大。

## 二、数据存放与 k8s 对象

| 项目 | 取值 |
|---|---|
| 数据目录 | `/mnt/hdd-data/volumes/<owner>-<name>`,由服务创建,权限 0755 |
| PV / PVC 名 | `hdd-<owner>-<name>`,两者同名 |
| PVC 命名空间 | `coder-workspaces` |
| PV 类型 | `local`,路径就是数据目录,`persistentVolumeReclaimPolicy: Retain` |
| 节点绑定 | `nodeAffinity: kubernetes.io/hostname In [ubuntu0002]`(服务 flag `-volume-node`,默认 `ubuntu0002`) |
| StorageClass | `coder-hdd`:`provisioner: kubernetes.io/no-provisioner`(无动态供给),`volumeBindingMode: WaitForFirstConsumer`,`reclaimPolicy: Retain` |
| 标签 | `coder-hdd-volume=true`、`coder-hdd-owner=<owner>`、`coder-hdd-name=<name>` |
| 注解 | `coder.com/hdd-volume-path=<数据目录>`、`coder.com/hdd-volume-size-gb=<size_gb>` |
| 服务 | 宿主 systemd `cluster-capacity.service`,监听 `:3999`,用 client-go 直接操作 k8s API;flag 默认 `-volume-root /mnt/hdd-data/volumes`、`-volume-storage-class coder-hdd`、`-namespace coder-workspaces` |

创建顺序:建目录、建 PV、建 PVC;PVC 创建失败时回滚删除 PV。owner 与 name 不能为空,也不能含空格或斜杠。PVC 用 `volumeName` 直接指向同名 PV,容量请求与 PV 一致。列表只取带 `coder-hdd-volume=true` 标签的 PVC;对手工创建、缺少 owner/name 标签的卷,从 `hdd-<owner>-<name>` 反推名称。

## 三、配额

- 机制:XFS project quota,HDD 必须以 `prjquota` 挂载(宿主当前已启用,`findmnt` 可见并已由 timer 应用)。project id = `crc32(PVC 名) % 4000000 + 100`,硬上限 `bhard` 取 PVC 的 `spec.resources.requests.storage`,因此容器内 `df` 看到的是卷的申请值,写超由内核直接拒绝。
- 应用方式:只有定时脚本在应用:宿主 systemd timer `coder-hdd-volume-quota.timer`(`OnBootSec=3min`,`OnUnitActiveSec=1min`)触发 `coder-hdd-volume-quota.service`,执行 `/usr/local/sbin/coder-hdd-volume-quota.py --quiet`(仓库内 `deploy/scripts/` 与 `deploy/cluster-capacity/` 两份内容相同)。服务端创建卷时不设置配额。
- 卷目录来源:优先 PVC 注解 `coder.com/hdd-volume-path`,缺省由 PVC 名推导;脚本会补建缺失的目录,并对每个卷执行 `project -s` 与 `limit -p bhard=...`。
- 附带行为:脚本同时清理每个卷 `.uploads/` 下超过 24 小时的残留分片。HDD 未以 `prjquota` 挂载时脚本直接跳过;服务读 `/proc/mounts` 判断,并在 `/hdd` 返回 `prjquota_active`,前端据此提示硬配额尚未生效。
- 与工作区卷配额是两套独立机制:SSD 上的工作区卷由 `coder-workspace-quota.py` 处理。

## 四、模板挂载

- `deploy/coder-template-kubernetes/main.tf` 里的参数 `hdd_volume`(order 7,string,mutable,默认空)填的是卷的 PVC 名。
- 模板无条件挂载:`local.hdd_claim_name = trimspace(参数) == "" ? "hdd-none" : trimspace(参数)`;deployment 里 volume `hdd` 引用该 PVC,`volume_mount` 固定挂到容器内 `/mnt/data`(卷名不进路径)。
- 参数留空时挂占位卷 `hdd-none`,定义在 `deploy/hdd-volumes/placeholder.yaml`:1GiB 的 PV + PVC,目录 `/mnt/hdd-data/volumes/_placeholder`;它同样带 `coder-hdd-volume=true` 标签,因此也会被配额脚本按 1GiB 处理。
- 权限:Pod `security_context` 设 `run_as_user = 1000`、`fs_group = 1000`,容器 `run_as_user = 1000`,卷内文件对 coder(uid 1000)可读写。
- `hdd_volume` 是 mutable 参数,改它会让工作区重启;卷里的数据不受影响。

## 五、HTTP 接口

服务对所有响应带 `Access-Control-Allow-Origin: *` 与 `Cache-Control: no-store`,跨域预检由服务自己回应;`/capacity` 有 3 秒缓存。

| 方法 | 路径 | 行为 |
|---|---|---|
| GET | `/capacity` | CPU / 内存 / GPU / 磁盘容量。HDD 卷不计入:统计 PVC 时跳过带 `coder-hdd-volume=true` 标签的,`disk` 只是工作区磁盘账 |
| GET | `/hdd` | HDD 总量、已用、空闲、卷数量、`prjquota_active`(对卷根目录 statfs) |
| GET | `/volumes` | `{"volumes":[...]}`,每项含 `name`、`owner`、`pvc`、`path`、`size_gb`、`phase`、`in_use_by`(正在运行且挂了该 PVC 的 Pod 名) |
| POST | `/volumes` | 建卷,请求体 `{owner, name, size_gb}`;`size_gb <= 0` 返回 400,成功返回 201 与卷对象。不校验 100 到 1000 GB 或每人 1000 GB |
| GET | `/volumes/{pvc}` | 单个卷 |
| DELETE | `/volumes/{pvc}` | 删卷。请求体 `confirm` 必须等于 URL 里的 PVC 名,否则 400;卷正被运行中的工作区使用时返回 409。默认只删 PVC + PV,数据目录保留;`?purge=true` 才连目录一起删。删除前会清掉卷内 `.uploads/` |
| POST | `/volumes/{pvc}/uploads?filename=&size=` | 开始上传,返回 `upload_id` 与 `offset` |
| GET 或 HEAD | `/volumes/{pvc}/uploads/{id}` | 查询已收字节数,返回体与 `X-Upload-Offset` 头 |
| PATCH | `/volumes/{pvc}/uploads/{id}?offset=` | 追加分片;offset 与服务端不一致时返回 409 与真实 offset |
| DELETE | `/volumes/{pvc}/uploads/{id}` | 取消上传,删掉分片 |
| POST | `/volumes/{pvc}/uploads/{id}/complete?on_conflict=overwrite\|rename` | 落盘;同名冲突未指定处理方式时返回 409 与 `exists: true` |

`GET /` 另有一个服务自带的极简卷申请页(`deploy/cluster-capacity/page.go`),功能与前端 Volumes 页重叠。

## 六、上传

- 前端按 8 MiB 分片上传,服务端单次请求上限 64 MiB;分片以偏移量追加,可断点续传,失败重试 3 次(退避 1s、2s)。
- 上传状态保存在卷目录下的隐藏目录 `.uploads/<id>.<文件名>.part` 里,服务重启不丢进度;分片文件名过滤路径穿越与隐藏文件,完成时用 `rename` 原子移到卷根目录(覆盖时即原子替换),用户看不到半截文件。
- 开始上传时如果带 `size`,会先对卷目录 statfs 比对剩余额度,超出直接返回 413;配额生效后 statfs 报的就是卷的剩余额度。
- 同名冲突由用户在界面上选择:覆盖(rename 原子替换)、保留两者(自动改成 `name(1).ext`)、取消。
- 中断上传的残留分片按 24 小时清理:服务自身每小时扫一遍各卷的 `.uploads/`,配额脚本每分钟也会顺带清理;删除卷时同样会清掉 `.uploads/`。

## 七、前端

- `/volumes` 页面(`site/src/pages/VolumesPage/VolumesPage.tsx`):常量 `MIN_GB = 100`、`MAX_GB = 1000`、`BUDGET_GB = 1000`(每人合计),创建前在页面上校验,服务端不校验。建卷后最多轮询 20 次、间隔 1.5 秒等待 `phase` 变成 `Bound`。
- 页面上方容量条读 `/hdd`,仅此处显示 HDD 余量;申请页的容量条只有 CPU / 内存 / GPU / 磁盘(`/capacity`)。
- 工作区创建表单里的卷选择器(`site/src/modules/clusterCapacity/HddVolumeField.tsx`)从 `/volumes` 拉取,只显示 `owner` 等于当前用户的卷;服务不可达时退化成手填 PVC 名的文本框。列表里的「使用中」只是标注,不禁止选择。
- 删除时要求用户输入卷名,提交的请求体 `confirm` 是 PVC 名。

## 八、已知边界

- 单节点绑定:卷的 PV 绑定 `ubuntu0002`,只能挂到该节点上的工作区。
- 无冗余:HDD 由 4 块盘经 LVM 线性拼成,任何一块盘故障都会丢失全部卷数据;这不是备份。
- XFS 不能缩小,卷容量只能通过重建卷调整;PV/PVC 的申请值本身就是配额值。
- 误删不可恢复:界面删除默认保留数据目录,但 PV/PVC 已删除,数据不再可挂载;`?purge=true` 连目录一起删。不做回收站。
- 同一个卷允许同时挂到多个工作区(有意为之,RWO 在同节点不阻止多挂),并发读写冲突由用户自行保证,服务不拦截;「使用中」只是提示。删除在有运行中工作区使用时会被拒绝,必须先停工作区。
- HDD 不计入 `/capacity`,不参与工作区申请页的容量条;卷的配额是整个卷共享一份,由所有挂载它的工作区共同使用。
- HDD 随机 IO 差,只适合冷数据。
