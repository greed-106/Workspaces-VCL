import { useState } from "react";
import { useQuery } from "react-query";
import type { DashboardInstance } from "#/api/api";
import { dashboardInstances } from "#/api/queries/dashboard";
import { Alert } from "#/components/Alert/Alert";
import { ErrorAlert } from "#/components/Alert/ErrorAlert";
import { Badge } from "#/components/Badge/Badge";
import { Button } from "#/components/Button/Button";
import {
	Dialog,
	DialogContent,
	DialogHeader,
	DialogTitle,
} from "#/components/Dialog/Dialog";
import { Skeleton } from "#/components/Skeleton/Skeleton";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "#/components/Table/Table";
import { AgentGpuHistory } from "#/modules/resources/AgentGpuHistory";

const formatUptime = (seconds: number): string => {
	if (seconds < 60) {
		return `${seconds} 秒`;
	}
	const minutes = Math.floor(seconds / 60);
	if (minutes < 60) {
		return `${minutes} 分钟`;
	}
	const hours = Math.floor(minutes / 60);
	if (hours < 24) {
		return `${hours} 小时 ${minutes % 60} 分`;
	}
	return `${Math.floor(hours / 24)} 天 ${hours % 24} 小时`;
};

const formatGiB = (value: number): string => `${value.toFixed(1)} GiB`;

/**
 * 正在运行的实例看板,所有登录用户可见。这里只展示信息:没有启动/停止/删除等操作,
 * 也不提供 VS Code、终端等实例入口(那些仍然只有工作区属主能使用)。
 */
const DashboardPage: React.FC = () => {
	const instancesQuery = useQuery(dashboardInstances());
	const [gpuHistoryFor, setGpuHistoryFor] = useState<DashboardInstance>();

	return (
		<div className="px-4 py-6 sm:px-6 lg:px-10 lg:py-10">
			<div className="mx-auto flex w-full max-w-(--breakpoint-xl) flex-col gap-6">
				<header className="flex flex-wrap items-end justify-between gap-3">
					<div>
						<h1 className="m-0 text-2xl font-semibold">Dashboard</h1>
						<p className="m-0 mt-1 text-sm text-content-secondary">
							所有用户正在运行的实例、配置与当前用量(只读,每 15 秒刷新)
						</p>
					</div>
					<Button
						variant="outline"
						size="sm"
						onClick={() => instancesQuery.refetch()}
						disabled={instancesQuery.isFetching}
					>
						刷新
					</Button>
				</header>

				{instancesQuery.isError ? (
					<ErrorAlert error={instancesQuery.error} />
				) : instancesQuery.isLoading ? (
					<Skeleton width="100%" height={320} className="rounded-lg" />
				) : (instancesQuery.data ?? []).length === 0 ? (
					<Alert severity="info">当前没有正在运行的实例。</Alert>
				) : (
					<div className="overflow-hidden rounded-lg border border-solid border-border">
						<Table aria-label="正在运行的实例">
							<TableHeader>
								<TableRow>
									<TableHead className="pl-5">用户 / 实例</TableHead>
									<TableHead>状态</TableHead>
									<TableHead>配置</TableHead>
									<TableHead>当前用量</TableHead>
									<TableHead>运行时间</TableHead>
									<TableHead className="pr-5 text-right">GPU 历史</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{(instancesQuery.data ?? []).map((instance) => (
									<TableRow key={instance.workspace_id}>
										<TableCell className="pl-5">
											<span className="text-content-secondary">
												{instance.username}
											</span>
											<span className="text-content-secondary"> / </span>
											<span className="font-medium">
												{instance.workspace_name}
											</span>
										</TableCell>
										<TableCell>
											<Badge variant="green" size="sm">
												{instance.status}
											</Badge>
										</TableCell>
										<TableCell className="text-xs text-content-secondary">
											{instance.cpu_limit.toFixed(0)} 核 ·{" "}
											{instance.memory_gib.toFixed(0)} GiB 内存 ·{" "}
											{instance.gpu_count === 0
												? "无 GPU"
												: `${instance.gpu_count} 张 GPU`}{" "}
											· {instance.disk_gib} GiB 磁盘
										</TableCell>
										<TableCell className="text-xs">
											{instance.used_cpu_cores.toFixed(2)} 核 ·{" "}
											{formatGiB(instance.used_memory_gib)} ·{" "}
											{formatGiB(instance.disk_used_gib)}
										</TableCell>
										<TableCell className="text-xs">
											{formatUptime(instance.uptime_seconds)}
										</TableCell>
										<TableCell className="pr-5 text-right">
											<Button
												size="sm"
												variant="outline"
												disabled={instance.gpu_count === 0}
												onClick={() => setGpuHistoryFor(instance)}
											>
												GPU 历史
											</Button>
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					</div>
				)}
			</div>

			<Dialog
				open={gpuHistoryFor !== undefined}
				onOpenChange={(open) => {
					if (!open) {
						setGpuHistoryFor(undefined);
					}
				}}
			>
				<DialogContent className="max-w-4xl">
					<DialogHeader>
						<DialogTitle>
							{`${gpuHistoryFor?.username ?? ""} / ${gpuHistoryFor?.workspace_name ?? ""} 的 GPU 历史`}
						</DialogTitle>
					</DialogHeader>
					{gpuHistoryFor && (
						<AgentGpuHistory workspaceId={gpuHistoryFor.workspace_id} />
					)}
				</DialogContent>
			</Dialog>
		</div>
	);
};

export default DashboardPage;
