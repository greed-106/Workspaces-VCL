import { useCallback, useEffect, useState } from "react";
import { Button } from "#/components/Button/Button";
import { useAuthenticated } from "#/hooks/useAuthenticated";
import { VolumeUpload } from "./VolumeUpload";

/**
 * HDD volumes: large cold-storage volumes that outlive workspaces and can be
 * attached to any of the owner's workspaces, where they show up at /mnt/data.
 *
 * The list comes from the cluster-capacity service. The 100-1000 GB range and
 * the 1000 GB per-user budget are enforced only here, so the CLI and API stay
 * unrestricted.
 */

const PORT = 3999;
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
	phase: string;
	in_use_by?: string[];
};

const endpoint = (path = "") =>
	`http://${window.location.hostname}:${PORT}/volumes${path}`;

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const errorText = (error: unknown) =>
	error instanceof Error ? error.message : String(error);

const formatSize = (gb: number) =>
	gb >= 1024 ? `${(gb / 1024).toFixed(2)} TB` : `${Math.round(gb)} GB`;

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
	useEffect(() => {
		const load = async () => {
			try {
				const res = await fetch(
					`http://${window.location.hostname}:${PORT}/hdd`,
					{ cache: "no-store" },
				);
				setInfo((await res.json()) as HddInfo);
			} catch {
				setInfo(null);
			}
		};
		void load();
		const timer = window.setInterval(() => void load(), 15000);
		return () => window.clearInterval(timer);
	}, []);
	if (!info) {
		return null;
	}
	return (
		<div
			role="status"
			className="flex flex-wrap items-center gap-x-4 gap-y-1 rounded-md border border-solid border-border-default bg-surface-secondary px-3 py-2 text-xs text-content-secondary"
		>
			<span className="font-medium text-content-primary">HDD 容量</span>
			<span>总计 {formatSize(info.total_gb)}</span>
			<span>已用 {formatSize(info.used_gb)}</span>
			<span>空闲 {formatSize(info.free_gb)}</span>
			<span>卷 {info.volumes} 个</span>
			{!info.prjquota_active && <span>硬配额尚未生效</span>}
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
									<th className="py-2 pr-4 font-medium">状态</th>
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
										<td className="py-2 pr-4">{formatSize(volume.size_gb)}</td>
										<td className="py-2 pr-4">{volume.phase}</td>
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
			</div>
		</div>
	);
};

export default VolumesPage;
