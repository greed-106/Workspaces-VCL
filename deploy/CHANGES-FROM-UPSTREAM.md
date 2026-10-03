# 相对 Coder 上游的改动

## 基线

| 项目 | 值 |
| --- | --- |
| 上游仓库 | `coder/coder` |
| 基线提交 | `95c9dcaa17`(2026-09-30) |
| 本机构建版本号 | `v2.37.3-devel`(控制面 UI 左下角显示) |
| 改动范围 | 仅前端 `site/`:28 个文件修改、6 个新增、646 个删除 |
| Go 后端 | 未改动 |

本仓库是一次性快照,不含上游 git 历史。若要在一个全新的上游检出上重现这些改动,按下文逐条操作即可;逐条内容也都是 WebUI 定制清单。

## 一、新增文件(6 个)

| 文件 | 作用 |
| --- | --- |
| `site/src/pages/VolumesPage/VolumesPage.tsx` | 数据卷页面:申请、列表、删除,每人只能看到自己的卷 |
| `site/src/pages/VolumesPage/VolumeUpload.tsx` | 分片上传器:断点续传、失败重试、同名冲突由用户选择 |
| `site/src/modules/clusterCapacity/clusterCapacity.ts` | 实时容量数据源(轮询 `:3999/capacity`)与 `liveMaxFor` |
| `site/src/modules/clusterCapacity/HddVolumeField.tsx` | 数据卷选择器(只列当前用户已绑定的卷) |
| `site/src/pages/CreateWorkspacePage/ClusterCapacityBar.tsx` | 申请页与参数页顶部的容量条 |
| `site/src/modules/workspaces/DynamicParameter/parameterUiHints.ts` | 参数加减按钮的上下限提示文案 |

## 二、修改文件(28 个)

参数与申请流程:

- `site/src/modules/workspaces/DynamicParameter/DynamicParameter.tsx` 与同名 `.test.tsx`
- `site/src/pages/CreateWorkspacePage/CreateWorkspacePageView.tsx`
- `site/src/pages/WorkspaceSettingsPage/WorkspaceParametersPage/WorkspaceParametersPageView.tsx`

导航与页面骨架(去掉入口、接入数据卷页):

- `site/src/router.tsx`(删除 Agents、AI 设置、External Authentication 路由;新增 `/volumes`)
- `site/src/modules/dashboard/Navbar/Navbar.tsx`、`NavbarView.tsx`、`NavbarView.stories.tsx`
- `site/src/modules/dashboard/Navbar/AdminSettings.tsx`(管理菜单去掉 AI 项)
- `site/src/modules/dashboard/Navbar/UserDropdown/UserDropdown.tsx`、`UserDropdownContent.tsx`(去掉试用 CTA 与外链)
- `site/src/modules/dashboard/Navbar/MobileMenu.tsx`、`MobileMenu.stories.tsx`
- `site/src/modules/management/DeploymentSidebarView.tsx`(去掉 Licenses 与 External Authentication)
- `site/src/pages/UserSettingsPage/Sidebar.tsx`(去掉 External Authentication)

去掉 GitHub 登录、商业版与桌面版 VS Code 入口:

- `site/src/pages/LoginPage/OAuthSignInForm.tsx`
- `site/src/pages/DeploymentSettingsPage/UserAuthSettingsPage/UserAuthSettingsPageView.tsx`
- `site/src/pages/UserSettingsPage/SecurityPage/SingleSignOnSection.tsx`
- `site/src/pages/WorkspacePage/WorkspaceBuildFailedAlert.tsx`(去掉 Debug with Coder Agents)
- `site/src/pages/WorkspacesPage/WorkspacesTable.tsx`(去掉 Agent 徽标与桌面版入口)
- `site/src/modules/resources/AgentRow.tsx`、`AgentRowPreview.tsx`(去掉桌面版 VS Code 入口)

主题、通知、构建:

- `site/src/theme/index.ts`(默认主题 light)
- `site/src/pages/UserSettingsPage/NotificationsPage/NotificationsPage.tsx`(去掉 AI 相关通知分组)
- `site/src/serviceWorker.ts`(通知兜底路径改到 `/workspaces`)
- `site/vite.config.mts`、`site/scripts/check-compiler.mjs`(React Compiler 目标目录与代码同步)
- `site/src/modules/permissions/index.ts`(去掉已删页面对应的权限辅助函数)

## 三、删除的文件(646 个)

| 目录 | 文件数 | 说明 |
| --- | --- | --- |
| `site/src/pages/AgentsPage/` | 490 | Agents 与聊天界面整体移除 |
| `site/src/pages/AISettingsPage/` | 134 | AI 设置页整体移除 |
| `site/src/pages/DeploymentSettingsPage/` | 6 | `AIGovernanceSettingsPage/`、`ExternalAuthSettingsPage/`、`LicensesSettingsPage/` |
| `site/src/modules/resources/` | 6 | 桌面版 VS Code 与 devcontainer 相关组件 |
| `site/src/modules/management/` | 4 | `AISettingsSidebar*` |
| `site/src/pages/UserSettingsPage/` | 3 | `ExternalAuthPage/` |
| `site/src/modules/workspaces/` | 1 | 未再使用的参数组件 |
| `site/src/api/queries/chats.test.ts` | 1 | Agents 移除后不再需要 |
| `site/e2e/tests/agents/chatSearch.spec.ts` | 1 | 同上 |

保留但不再暴露入口:`/external-auth/:provider`(工作区 Git 授权回调)与 `ExternalAuthButton`(创建工作区时的 Authorize 按钮)。

## 四、在全新上游检出上重现的步骤

1. 检出上游基线提交:`git checkout 95c9dcaa17`。
2. 按第一节新增 6 个文件(可直接从本仓库 `site/` 对应路径复制)。
3. 按第二节修改 28 个文件(可直接从本仓库 `site/` 对应路径覆盖)。
4. 按第三节删除对应目录与文件,并在 `site/src/router.tsx` 中删除它们的 lazy import 与路由。
5. 重新构建前端与控制面:`make site/out/index.html build/coder_linux_amd64`。
6. 重启控制面,浏览器强制刷新一次(service worker 会缓存旧资源)。

模板、容量服务、镜像与集群侧的改动不在本仓库的 `site/` 内,见 `deploy/README.md` 与 `deploy/` 下的配置文件。
