import { cn } from "cn";
import { RefreshCwIcon } from "lucide-react";
import { Button } from "#/components/Button/Button";
import {
	Tooltip,
	TooltipContent,
	TooltipProvider,
	TooltipTrigger,
} from "#/components/Tooltip/Tooltip";
import {
	refreshClusterCapacity,
	useClusterCapacity,
} from "#/modules/clusterCapacity/clusterCapacity";

/**
 * Live cluster capacity for the workspace forms.
 *
 * Layout is deliberately stable: the metrics wrap inside their own block while
 * the refresh control keeps its place at the end of the row, so the row never
 * re-flows into a different shape as the numbers change. The per-model detail
 * and the "last updated" time live in tooltips instead of taking up room.
 *
 * CPU is deliberately not shown: workspaces are budgeted with an overcommit
 * factor (CPU is compressible), so a "free cores" figure would mislead. The
 * budget still guards the 8/16/32 options and the build-time check.
 *
 * Parameter options only carry a snapshot taken when the template was pushed, so
 * this bar is what keeps the forms honest. It refreshes every ten seconds and
 * can be refreshed by hand; when the endpoint is unreachable it renders nothing.
 */
export const ClusterCapacityBar: React.FC<{ className?: string }> = ({
	className,
}) => {
	const { data, loading, updatedAt } = useClusterCapacity();

	if (!data) {
		return null;
	}

	const models = Object.entries(data.gpu.models ?? {})
		.map(
			([name, model]) =>
				`${name.replace("NVIDIA-", "")} ${model.free}/${model.total}`,
		)
		.join("、");

	const updatedLabel = updatedAt
		? `最后更新 ${new Date(updatedAt).toLocaleTimeString("zh-CN", { hour12: false })}`
		: "点击刷新";

	return (
		<div
			role="status"
			className={cn(
				"flex items-center gap-4 rounded-md border border-solid border-border-default bg-surface-secondary px-3 py-2 text-xs text-content-secondary",
				className,
			)}
		>
			{/* Metrics: wrap inside this block only. */}
			<div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-4 gap-y-1">
				<span className="font-medium text-content-primary">集群当前空闲</span>
				<TooltipProvider delayDuration={200}>
					<Tooltip>
						<TooltipTrigger asChild>
							<span className="cursor-help">GPU {data.gpu.free} 张</span>
						</TooltipTrigger>
						<TooltipContent side="bottom">
							{models || "没有探测到可用型号"}
						</TooltipContent>
					</Tooltip>
				</TooltipProvider>
				<span>内存 {data.memory.free_gib} GB</span>
				<span>磁盘 {Math.round(data.disk.free_gb)} GB</span>
			</div>

			{/* Refresh control: fixed cell at the end of the row, never wraps. */}
			<div className="flex shrink-0 items-center">
				<TooltipProvider delayDuration={200}>
					<Tooltip>
						<TooltipTrigger asChild>
							<Button
								variant="subtle"
								size="icon"
								className="size-6"
								aria-label="刷新集群容量"
								disabled={loading}
								onClick={() => {
									void refreshClusterCapacity();
								}}
							>
								<RefreshCwIcon className={loading ? "animate-spin" : ""} />
							</Button>
						</TooltipTrigger>
						<TooltipContent side="bottom">{updatedLabel}</TooltipContent>
					</Tooltip>
				</TooltipProvider>
			</div>
		</div>
	);
};
