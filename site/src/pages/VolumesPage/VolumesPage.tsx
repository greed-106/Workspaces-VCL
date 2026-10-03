import { cn } from "cn";
import { RefreshCwIcon } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { Button } from "#/components/Button/Button";
import {
	Tooltip,
	TooltipContent,
	TooltipProvider,
	TooltipTrigger,
} from "#/components/Tooltip/Tooltip";
import { useAuthenticated } from "#/hooks/useAuthenticated";
import { capacityUrl } from "#/modules/clusterCapacity/clusterCapacity";
import { VolumeUpload } from "./VolumeUpload";

/**
 * HDD volumes: large cold-storage volumes that outlive workspaces and can be
 * attached to any of the owner's workspaces, where they show up at /mnt/data.
 *
 * The list comes from the cluster-capacity service. The 100-1000 GB range and
 * the 1000 GB per-user budget are enforced only here, so the CLI and API stay
 * unrestricted.
 */

const MIN_GB = 100;
const MAX_GB = 1000;
const BUDGET_GB = 1000;
const BIND_POLLS = 20;
const BIND_INTERVAL_MS = 1500;

type Volume = {
	pvc: string;
	name: string;
	owner: string;
	path: string;
	size_gb: number;
	/** 实际占用(null = 定时器快照还不可用) */
	used_gb: number | null;
	/** XFS project quota 上的生效上限(申请值被调整后以它为准) */
	limit_gb: number;
	phase: string;
	in_use_by?: string[];
};

const endpoint = (path = "") => capacityUrl(`/volumes${path}`);

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const errorText = (error: unknown) =>
	error instanceof Error ? error.message : String(error);

const formatSize = (gb: number) =>
	gb >= 1024 ? `${(gb / 1024).toFixed(2)} TB` : `${Math.round(gb)} GB`;

/** PVC 状态的中文说法;悬停里会给出原始值与该状态的含义。 */
const PHASE_LABELS: Record<string, { label: string; hint: string }> = {
	Bound: {
		label: "已绑定",
		hint: "Bound:已绑定到存储对象,可以挂到工作区使用",
	},
	Pending: {
		label: "待绑定",
		hint: "Pending:刚创建、还在绑定,通常几秒内完成;若一直停在这里说明存储对象没建成功",
	},
	Lost: {
		label: "已丢失",
		hint: "Lost:绑定的存储对象被删掉了,数据目录还在但卷不可用,需要管理员重建存储对象",
	},
	Terminating: {
		label: "删除中",
		hint: "Terminating:正在删除,稍后会从列表消失",
	},
};

const PhaseLabel: React.FC<{ phase: string }> = ({ phase }) => {
	const info = PHASE_LABELS[phase];
	return (
		<TooltipProvider delayDuration={200}>
			<Tooltip>
				<TooltipTrigger asChild>
					<span className="cursor-help">{info?.label ?? phase}</span>
				</TooltipTrigger>
				<TooltipContent side="bottom">
					{info?.hint ?? `k8s 状态:${phase}`}
				</TooltipContent>
			</Tooltip>
		</TooltipProvider>
	);
};

const formatUsed = (gb: number | null) => {
	if (gb === null) {
		return "—";
	}
	if (gb >= 1) {
		return `${gb.toFixed(gb >= 10 ? 1 : 2)} GB`;
	}
	return `${Math.round(gb * 1024)} MB`;
};

/** 细长用量条,颜色与状态条一致(超过 90% 转警示色)。 */
const UsageBar: React.FC<{ used: number | null; total: number }> = ({
	used,
	total,
}) => {
	if (used === null || total <= 0) {
		return <span className="text-content-secondary">—</span>;
	}
	const pct = Math.min(100, (used / total) * 100);
	return (
		<div className="flex items-center gap-2">
			<div className="h-1.5 w-24 overflow-hidden rounded-full bg-surface-secondary">
				<div
					className={cn(
						"h-full rounded-full",
						pct >= 90 ? "bg-border-destructive" : "bg-content-primary",
					)}
					style={{ width: `${Math.max(pct, pct > 0 ? 2 : 0)}%` }}
				/>
			</div>
			<span className="tabular-nums">
				{pct < 1 && pct > 0 ? "<1" : pct.toFixed(0)}%
			</span>
		</div>
	);
};

/** Throws with the service's message so callers can just try/catch. */
const request = async (path: string, init?: RequestInit): Promise<unknown> => {
	const response = await fetch(endpoint(path), { cache: "no-store", ...init });
	const body = (await response.json().catch(() => ({}))) as { error?: string };
	if (!response.ok) {
		throw new Error(body.error ?? `请求失败(HTTP ${response.status})`);
	}
	return body;
};

/** Waits for the volume to bind and returns the log line describing the result. */
const waitForBound = async (pvc: string): Promise<string> => {
	for (let attempt = 0; attempt < BIND_POLLS; attempt++) {
		await sleep(BIND_INTERVAL_MS);
		try {
			const volume = (await request(`/${pvc}`)) as Volume;
			if (volume.phase === "Bound") {
				return "✓ 绑定完成,可以在建工作区时挂载它了(容器内路径 /mnt/data)";
			}
		} catch {
			// 单次查询失败不打断等待,下一轮继续。
		}
	}
	return "… 仍在绑定中,稍后会自动完成,可刷新查看";
};

type HddInfo = {
	total_gb: number;
	used_gb: number;
	free_gb: number;
	volumes: number;
	prjquota_active: boolean;
};

const HddCapacity: React.FC = () => {
	const [info, setInfo] = useState<HddInfo | null>(null);
	const [loading, setLoading] = useState(false);

	const load = useCallback(async () => {
		setLoading(true);
		try {
			const res = await fetch(capacityUrl("/hdd"), { cache: "no-store" });
			setInfo((await res.json()) as HddInfo);
		} catch {
			setInfo(null);
		} finally {
			setLoading(false);
		}
	}, []);

	useEffect(() => {
		void load();
		const timer = window.setInterval(() => void load(), 10000);
		return () => window.clearInterval(timer);
	}, [load]);

	if (!info) {
		return null;
	}
	const usedPct =
		info.total_gb > 0 ? Math.min(100, (info.used_gb / info.total_gb) * 100) : 0;
	return (
		<div
			role="status"
			className="flex items-center gap-4 rounded-md border border-solid border-border-default bg-surface-secondary px-3 py-2 text-xs text-content-secondary"
		>
			<div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-4 gap-y-1">
				<span className="font-medium text-content-primary">
					HDD 数据卷容量(hdd-data)
				</span>
				<span>总计 {formatSize(info.total_gb)}</span>
				<span>已用 {formatSize(info.used_gb)}</span>
				<span>空闲 {formatSize(info.free_gb)}</span>
				<span>卷 {info.volumes} 个</span>
				{!info.prjquota_active && <span>硬配额尚未生效</span>}
				<div className="flex min-w-40 items-center gap-2">
					<div className="h-1.5 w-32 overflow-hidden rounded-full bg-surface-primary">
						<div
							className={cn(
								"h-full rounded-full",
								usedPct >= 90 ? "bg-border-destructive" : "bg-content-primary",
							)}
							style={{ width: `${Math.max(usedPct, usedPct > 0 ? 2 : 0)}%` }}
						/>
					</div>
					<span className="tabular-nums">
						已用 {usedPct < 1 && usedPct > 0 ? "<1" : usedPct.toFixed(0)}%
					</span>
				</div>
			</div>
			<Button
				variant="subtle"
				size="icon"
				className="size-6 shrink-0"
				aria-label="刷新 HDD 容量"
				disabled={loading}
				onClick={() => void load()}
			>
				<RefreshCwIcon className={loading ? "animate-spin" : ""} />
			</Button>
		</div>
	);
};

const VolumesPage: React.FC = () => {
	const { user } = useAuthenticated();
	const [volumes, setVolumes] = useState<Volume[]>([]);
	const [name, setName] = useState("");
	const [size, setSize] = useState(MIN_GB);
	const [busy, setBusy] = useState(false);
	const [log, setLog] = useState<string[]>([]);
	const [message, setMessage] = useState<string | null>(null);
	const [uploadTarget, setUploadTarget] = useState("");

	const mine = volumes.filter((volume) => volume.owner === user.username);
	const boundVolumes = mine.filter((volume) => volume.phase === "Bound");
	useEffect(() => {
		if (!boundVolumes.some((volume) => volume.pvc === uploadTarget)) {
			setUploadTarget(boundVolumes[0]?.pvc ?? "");
		}
	}, [boundVolumes, uploadTarget]);
	const usedGB = mine.reduce((sum, volume) => sum + volume.size_gb, 0);

	const appendLog = (line: string) => setLog((lines) => [...lines, line]);

	const load = useCallback(async () => {
		try {
			const data = (await request("")) as { volumes?: Volume[] };
			setVolumes(data.volumes ?? []);
		} catch {
			setVolumes([]);
		}
	}, []);

	useEffect(() => {
		void load();
		const timer = window.setInterval(() => void load(), 15000);
		return () => window.clearInterval(timer);
	}, [load]);

	const validate = (): string | null => {
		if (!name.trim()) {
			return "请填写卷名";
		}
		if (size < MIN_GB || size > MAX_GB) {
			return `容量需要在 ${MIN_GB} - ${MAX_GB} GB 之间`;
		}
		if (usedGB + size > BUDGET_GB) {
			return `你已申请 ${Math.round(usedGB)} GB,再加 ${size} GB 会超过每人 ${BUDGET_GB} GB 的上限`;
		}
		return null;
	};

	const create = async () => {
		setMessage(validate());
		if (validate()) {
			return;
		}
		const volumeName = name.trim();
		setBusy(true);
		setLog([`正在申请 ${volumeName}(${size} GB)…`]);
		try {
			const created = (await request("", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({
					owner: user.username,
					name: volumeName,
					size_gb: size,
				}),
			})) as Volume;
			setName("");
			appendLog(`✓ 数据目录已建好:${created.path}`);
			appendLog("✓ 存储对象已创建(存储卷 / 存储卷声明)");
			appendLog("… 等待绑定,通常几秒钟");
			appendLog(await waitForBound(created.pvc));
			setMessage(`已创建 ${volumeName}`);
		} catch (error) {
			appendLog(`✗ 申请失败:${errorText(error)}`);
			setMessage(`申请失败:${errorText(error)}`);
		} finally {
			setBusy(false);
			void load();
		}
	};

	const remove = async (volume: Volume) => {
		const typed = window.prompt(
			`删除后数据不可恢复。请输入卷名以确认:${volume.name}`,
		);
		if (typed !== volume.name) {
			return;
		}
		setBusy(true);
		setLog([`正在删除 ${volume.name}…`]);
		try {
			await request(`/${volume.pvc}`, {
				method: "DELETE",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ confirm: volume.pvc }),
			});
			appendLog(
				`✓ 已删除 ${volume.name}(数据目录保留,需要彻底清除请联系管理员)`,
			);
			setMessage(`已删除 ${volume.name}`);
		} catch (error) {
			appendLog(`✗ 删除失败:${errorText(error)}`);
			setMessage(`删除失败:${errorText(error)}`);
		} finally {
			setBusy(false);
			void load();
		}
	};

	return (
		<div className="px-4 sm:px-6 lg:px-10 py-6 lg:py-10">
			<div className="mx-auto flex w-full max-w-(--breakpoint-xl) flex-col gap-6">
				<header className="flex flex-col gap-2">
					<h1 className="text-3xl font-semibold m-0">Volumes</h1>
					<p className="text-sm text-content-secondary m-0">
						大容量冷数据卷,独立于工作区存在:可以挂到自己的任意工作区 (容器内路径{" "}
						<code className="text-content-primary">/mnt/data</code>
						),工作区删除后数据仍在。
					</p>
				</header>

				<HddCapacity />

				<section className="flex flex-col gap-4 rounded-md border border-solid border-border-default bg-surface-secondary p-4">
					<div className="flex flex-wrap items-end gap-4">
						<label className="flex flex-col gap-1 text-xs text-content-secondary">
							卷名
							<input
								className="w-48 rounded-md border border-solid border-border-default bg-surface-primary px-3 py-2 text-sm text-content-primary"
								value={name}
								placeholder="例如 dataset"
								onChange={(event) => setName(event.target.value)}
							/>
						</label>
						<label className="flex flex-col gap-1 text-xs text-content-secondary">
							容量(GB)
							<input
								type="number"
								min={MIN_GB}
								max={MAX_GB}
								step={100}
								className="w-32 rounded-md border border-solid border-border-default bg-surface-primary px-3 py-2 text-sm text-content-primary"
								value={size}
								onChange={(event) => setSize(Number(event.target.value))}
							/>
						</label>
						<Button onClick={() => void create()} disabled={busy}>
							申请卷
						</Button>
						<span className="text-xs text-content-secondary">
							每人上限 {BUDGET_GB} GB,已用 {Math.round(usedGB)} GB
						</span>
					</div>

					{busy && (
						<p role="status" className="text-sm m-0 text-content-secondary">
							处理中,请稍候…
						</p>
					)}
					{log.length > 0 && (
						<pre className="m-0 max-h-48 overflow-auto rounded-md border border-solid border-border-default bg-surface-primary p-3 text-xs leading-relaxed text-content-secondary">
							{log.join("\n")}
						</pre>
					)}
					{message && (
						<p role="status" className="text-sm m-0 text-content-secondary">
							{message}
						</p>
					)}
				</section>

				{mine.length > 0 && (
					<div className="flex flex-col gap-2">
						<h2 className="text-lg font-medium m-0">上传数据</h2>
						<select
							className="w-72 rounded-md border border-solid border-border-default bg-surface-primary px-3 py-2 text-sm text-content-primary"
							value={uploadTarget}
							onChange={(event) => setUploadTarget(event.target.value)}
						>
							{mine
								.filter((volume) => volume.phase === "Bound")
								.map((volume) => (
									<option key={volume.pvc} value={volume.pvc}>
										{volume.name}({formatSize(volume.size_gb)})
									</option>
								))}
						</select>
						{uploadTarget && <VolumeUpload volumePvc={uploadTarget} />}
					</div>
				)}

				<section className="flex flex-col gap-2">
					<h2 className="text-lg font-medium m-0">我的数据卷</h2>
					{mine.length === 0 ? (
						<p className="text-sm text-content-secondary m-0">还没有数据卷</p>
					) : (
						<table className="w-full border-collapse text-sm">
							<thead>
								<tr className="text-left text-content-secondary">
									<th className="py-2 pr-4 font-medium">卷名</th>
									<th className="py-2 pr-4 font-medium">容量</th>
									<th className="py-2 pr-4 font-medium">实际使用</th>
									<th className="py-2 pr-4 font-medium">使用率</th>
									<th className="py-2 pr-4 font-medium">
										<TooltipProvider delayDuration={200}>
											<Tooltip>
												<TooltipTrigger asChild>
													<span className="cursor-help">状态</span>
												</TooltipTrigger>
												<TooltipContent side="bottom" className="max-w-80">
													存储卷声明的绑定状态:已绑定 = 可用;待绑定 =
													还在绑定(几秒); 已丢失 = 存储对象被删、需要管理员处理
												</TooltipContent>
											</Tooltip>
										</TooltipProvider>
									</th>
									<th className="py-2 pr-4 font-medium">使用中</th>
									<th className="py-2 font-medium" />
								</tr>
							</thead>
							<tbody>
								{mine.map((volume) => (
									<tr
										key={volume.pvc}
										className="border-0 border-t border-solid border-border-default"
									>
										<td className="py-2 pr-4">{volume.name}</td>
										<td className="py-2 pr-4">
											{formatSize(volume.limit_gb || volume.size_gb)}
										</td>
										<td className="py-2 pr-4">{formatUsed(volume.used_gb)}</td>
										<td className="py-2 pr-4">
											<UsageBar
												used={volume.used_gb}
												total={volume.limit_gb || volume.size_gb}
											/>
										</td>
										<td className="py-2 pr-4">
											<PhaseLabel phase={volume.phase} />
										</td>
										<td className="py-2 pr-4">
											{(volume.in_use_by ?? []).length > 0
												? volume.in_use_by?.join(", ")
												: "—"}
										</td>
										<td className="py-2 text-right">
											<Button
												variant="subtle"
												size="sm"
												disabled={busy}
												onClick={() => void remove(volume)}
											>
												删除
											</Button>
										</td>
									</tr>
								))}
							</tbody>
						</table>
					)}
				</section>
				<section className="flex flex-col gap-2">
					<h2 className="text-lg font-medium m-0">全部数据卷</h2>
					<p className="text-xs text-content-secondary m-0">
						所有用户的卷,只读展示:申请大小、实际占用与使用率。删除等操作只在自己
						的卷上提供。
					</p>
					{volumes.length === 0 ? (
						<p className="text-sm text-content-secondary m-0">还没有数据卷</p>
					) : (
						<table className="w-full border-collapse text-sm">
							<thead>
								<tr className="text-left text-content-secondary">
									<th className="py-2 pr-4 font-medium">用户</th>
									<th className="py-2 pr-4 font-medium">卷名</th>
									<th className="py-2 pr-4 font-medium">申请大小</th>
									<th className="py-2 pr-4 font-medium">实际使用</th>
									<th className="py-2 pr-4 font-medium">使用率</th>
									<th className="py-2 pr-4 font-medium">
										<TooltipProvider delayDuration={200}>
											<Tooltip>
												<TooltipTrigger asChild>
													<span className="cursor-help">状态</span>
												</TooltipTrigger>
												<TooltipContent side="bottom" className="max-w-80">
													存储卷声明的绑定状态:已绑定 = 可用;待绑定 =
													还在绑定(几秒); 已丢失 = 存储对象被删、需要管理员处理
												</TooltipContent>
											</Tooltip>
										</TooltipProvider>
									</th>
									<th className="py-2 font-medium">使用中</th>
								</tr>
							</thead>
							<tbody>
								{volumes.map((volume) => (
									<tr
										key={volume.pvc}
										className="border-0 border-t border-solid border-border-default"
									>
										<td className="py-2 pr-4">{volume.owner}</td>
										<td className="py-2 pr-4">{volume.name}</td>
										<td className="py-2 pr-4">
											{formatSize(volume.limit_gb || volume.size_gb)}
										</td>
										<td className="py-2 pr-4">{formatUsed(volume.used_gb)}</td>
										<td className="py-2 pr-4">
											<UsageBar
												used={volume.used_gb}
												total={volume.limit_gb || volume.size_gb}
											/>
										</td>
										<td className="py-2 pr-4">
											<PhaseLabel phase={volume.phase} />
										</td>
										<td className="py-2">
											{(volume.in_use_by ?? []).length > 0
												? volume.in_use_by?.join(", ")
												: "—"}
										</td>
									</tr>
								))}
							</tbody>
						</table>
					)}
				</section>
			</div>
		</div>
	);
};

export default VolumesPage;
