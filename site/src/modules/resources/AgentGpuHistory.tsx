import dayjs from "dayjs";
import { useId, useState } from "react";
import { useQuery } from "react-query";
import {
	Bar,
	CartesianGrid,
	ComposedChart,
	Line,
	XAxis,
	YAxis,
} from "recharts";
import { workspaceGpuSeries, workspaceGpus } from "#/api/queries/gpuMetrics";
import { Button } from "#/components/Button/Button";
import {
	ChartContainer,
	ChartTooltip,
	ChartTooltipContent,
} from "#/components/Chart/Chart";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "#/components/Select/Select";
import { Skeleton } from "#/components/Skeleton/Skeleton";

const HOUR_OPTIONS = [1, 2, 4, 8, 12, 24];
const DAY_OPTIONS = [1, 2, 3, 5, 7];
const MIB_PER_GIB = 1024;

type RangeUnit = "hours" | "days";

type AgentGpuHistoryProps = {
	workspaceId: string;
};

/**
 * 工作区的 GPU 历史看板:一条曲线表示利用率(左轴),柱状表示显存占用(右轴)。
 * 同一时刻只显示一张卡,用下拉切换;切换时 Recharts 会在新旧数据之间做补间动画。
 * 数据由宿主机的 gpu-metrics 服务每分钟采集一次并写入 PostgreSQL,只记录申请了 GPU 的工作区。
 */
export const AgentGpuHistory: React.FC<AgentGpuHistoryProps> = ({
	workspaceId,
}) => {
	const hatchId = `gpu-mem-hatch-${useId().replace(/:/g, "")}`;
	const [selectedGpu, setSelectedGpu] = useState<string>();
	const [unit, setUnit] = useState<RangeUnit>("hours");
	const [hours, setHours] = useState(8);
	const [days, setDays] = useState(1);

	const gpusQuery = useQuery(workspaceGpus(workspaceId));
	const gpus = gpusQuery.data ?? [];
	// 默认展示第一张卡;不额外用 effect 同步,直接在选择值缺失时回退。
	const activeGpu = selectedGpu ?? gpus[0]?.uuid;
	const window = unit === "hours" ? hours : days * 24;

	const seriesQuery = useQuery({
		...workspaceGpuSeries(workspaceId, activeGpu ?? "", window),
		enabled: Boolean(activeGpu),
	});

	if (gpusQuery.isLoading) {
		return <Skeleton width="100%" height={260} className="rounded-lg" />;
	}
	// 没有 GPU 的工作区不显示这块内容。
	if (gpus.length === 0) {
		return null;
	}

	const points = (seriesQuery.data?.points ?? []).map((p) => ({
		t: dayjs(p.t).valueOf(),
		util_pct: p.util_pct,
		mem_gib: p.mem_used_mib / MIB_PER_GIB,
		mem_total_gib: p.mem_total_mib / MIB_PER_GIB,
	}));
	const totalGib =
		points.length > 0 ? points[points.length - 1].mem_total_gib : 12;
	// 右轴按该卡实际显存取整到整数 GiB,柱子不会顶到天花板。
	const memAxisMax = Math.ceil(totalGib);
	const current = gpus.find((g) => g.uuid === activeGpu) ?? gpus[0];

	return (
		<section className="mt-4 rounded-lg border border-solid border-border">
			<header className="flex flex-wrap items-center gap-3 border-0 border-b border-solid border-border px-5 py-3">
				<div className="mr-auto">
					<h3 className="m-0 text-sm font-semibold">GPU 使用历史</h3>
					<p className="m-0 mt-1 text-xs text-content-secondary">
						每分钟采样一次;曲线为利用率(左轴),柱状为显存占用(右轴)
					</p>
				</div>

				{gpus.length > 1 && (
					<Select value={activeGpu} onValueChange={setSelectedGpu}>
						<SelectTrigger
							aria-label="选择显卡"
							className="h-8 w-[180px] text-xs"
						>
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							{gpus.map((g) => (
								<SelectItem key={g.uuid} value={g.uuid}>
									显卡 {g.index + 1}(
									{(g.mem_total_mib / MIB_PER_GIB).toFixed(0)} GiB)
								</SelectItem>
							))}
						</SelectContent>
					</Select>
				)}

				<div className="flex items-center gap-1 rounded-md border border-solid border-border p-0.5">
					<Button
						size="sm"
						variant={unit === "hours" ? "outline" : "subtle"}
						onClick={() => setUnit("hours")}
					>
						小时
					</Button>
					<Button
						size="sm"
						variant={unit === "days" ? "outline" : "subtle"}
						onClick={() => setUnit("days")}
					>
						天
					</Button>
				</div>

				<Select
					value={String(unit === "hours" ? hours : days)}
					onValueChange={(v) =>
						unit === "hours" ? setHours(Number(v)) : setDays(Number(v))
					}
				>
					<SelectTrigger aria-label="时间范围" className="h-8 w-[92px] text-xs">
						<SelectValue />
					</SelectTrigger>
					<SelectContent>
						{(unit === "hours" ? HOUR_OPTIONS : DAY_OPTIONS).map((v) => (
							<SelectItem key={v} value={String(v)}>
								近 {v} {unit === "hours" ? "小时" : "天"}
							</SelectItem>
						))}
					</SelectContent>
				</Select>
			</header>

			{/* 斜线填充图案:显存柱用它填充,黑实线留给利用率曲线。 */}
			<svg aria-hidden width="0" height="0" className="absolute">
				<defs>
					<pattern
						id={hatchId}
						width="6"
						height="6"
						patternUnits="userSpaceOnUse"
						patternTransform="rotate(45)"
					>
						<line
							x1="0"
							y1="0"
							x2="0"
							y2="6"
							stroke="hsl(var(--content-secondary))"
							strokeWidth="1.5"
						/>
					</pattern>
				</defs>
			</svg>

			<div className="px-4 py-4">
				{seriesQuery.isLoading ? (
					<Skeleton width="100%" height={220} className="rounded" />
				) : points.length === 0 ? (
					<p className="m-0 py-10 text-center text-sm text-content-secondary">
						该时间范围内还没有采样数据
					</p>
				) : (
					<>
						<ChartContainer
							config={{
								util_pct: {
									label: "GPU 利用率",
									color: "hsl(var(--highlight-purple))",
								},
								mem_gib: {
									label: "显存占用",
									color: "hsl(var(--highlight-sky))",
								},
							}}
							className="h-[240px] w-full"
						>
							<ComposedChart
								data={points}
								margin={{ top: 8, right: 8, bottom: 0, left: 0 }}
							>
								<CartesianGrid vertical={false} strokeOpacity={0.15} />
								<XAxis
									dataKey="t"
									type="number"
									scale="time"
									domain={["dataMin", "dataMax"]}
									tickLine={false}
									axisLine={false}
									minTickGap={40}
									tickFormatter={(v: number) =>
										window > 24
											? dayjs(v).format("MM-DD HH:mm")
											: dayjs(v).format("HH:mm")
									}
								/>
								<YAxis
									yAxisId="util"
									domain={[0, 100]}
									tickLine={false}
									axisLine={false}
									width={38}
									tickFormatter={(v: number) => `${v}%`}
								/>
								<YAxis
									yAxisId="mem"
									orientation="right"
									domain={[0, memAxisMax]}
									tickLine={false}
									axisLine={false}
									width={46}
									tickFormatter={(v: number) => `${v}Gi`}
								/>
								<ChartTooltip
									content={
										<ChartTooltipContent
											labelFormatter={(_, payload) =>
												dayjs(payload?.[0]?.payload?.t).format("MM-DD HH:mm")
											}
										/>
									}
								/>
								<Bar
									yAxisId="mem"
									dataKey="mem_gib"
									fill={`url(#${hatchId})`}
									stroke="hsl(var(--content-secondary))"
									strokeWidth={1}
									maxBarSize={14}
									isAnimationActive
									animationDuration={700}
									animationEasing="ease-out"
								/>
								<Line
									yAxisId="util"
									dataKey="util_pct"
									type="monotone"
									stroke="hsl(var(--content-primary))"
									strokeWidth={2}
									dot={false}
									isAnimationActive
									animationDuration={700}
									animationEasing="ease-out"
								/>
							</ComposedChart>
						</ChartContainer>
						<p className="m-0 mt-2 text-xs text-content-secondary">
							显卡 {current.index + 1} · 宿主机编号 {current.host_index} · 显存{" "}
							{totalGib.toFixed(0)} GiB · 数据点 {points.length}
							{seriesQuery.data && seriesQuery.data.bucket_minutes > 1
								? `(已按 ${seriesQuery.data.bucket_minutes} 分钟聚合)`
								: ""}
						</p>
					</>
				)}
			</div>
		</section>
	);
};
