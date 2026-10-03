import { useEffect, useState } from "react";
import { useAuthenticated } from "#/hooks/useAuthenticated";
import { capacityUrl } from "./clusterCapacity";

/**
 * HDD volume picker.
 *
 * Coder freezes parameter options when a template is pushed, so the list of a
 * user's volumes cannot be delivered through the parameter definition. This
 * field talks to the cluster-capacity service directly instead, and falls back
 * to the plain text input when the service is unreachable.
 */

type HddVolume = {
	pvc: string;
	owner: string;
	size_gb: number;
	phase: string;
	in_use_by?: string[];
};

const label = (v: HddVolume) => {
	const size =
		v.size_gb >= 1024
			? `${(v.size_gb / 1024).toFixed(2)} TB`
			: `${Math.round(v.size_gb)} GB`;
	const busy = (v.in_use_by ?? []).length > 0 ? " · 使用中" : "";
	return `${v.pvc} · ${size} · ${v.owner}${busy}`;
};

export const HddVolumeField: React.FC<{
	id: string;
	value: string;
	onChange: (value: string) => void;
	disabled?: boolean;
}> = ({ id, value, onChange, disabled }) => {
	const [volumes, setVolumes] = useState<HddVolume[]>([]);
	const [failed, setFailed] = useState(false);
	const { user } = useAuthenticated();

	useEffect(() => {
		let alive = true;
		const endpoint = capacityUrl("/volumes");
		const load = async () => {
			try {
				const res = await fetch(endpoint, { cache: "no-store" });
				if (!res.ok) {
					throw new Error(String(res.status));
				}
				const data = (await res.json()) as { volumes?: HddVolume[] };
				if (alive) {
					// 每个人只看得到自己的卷。
					setVolumes(
						(data.volumes ?? []).filter((v) => v.owner === user.username),
					);
					setFailed(false);
				}
			} catch {
				if (alive) {
					setFailed(true);
				}
			}
		};
		void load();
		const timer = window.setInterval(() => void load(), 15000);
		return () => {
			alive = false;
			window.clearInterval(timer);
		};
	}, [user.username]);

	// 服务不可达时退回普通文本输入,不影响创建流程。
	if (failed) {
		return (
			<input
				id={id}
				className="w-full rounded-md border border-solid border-border-default bg-surface-primary px-3 py-2 text-sm"
				placeholder="hdd-<用户>-<卷名>"
				value={value}
				disabled={disabled}
				onChange={(event) => onChange(event.target.value)}
			/>
		);
	}

	// 当前值不在列表里(例如卷被删了)时保留它,避免把人选的东西悄悄改掉。
	const missing = value !== "" && !volumes.some((v) => v.pvc === value);

	return (
		<select
			id={id}
			className="w-full rounded-md border border-solid border-border-default bg-surface-primary px-3 py-2 text-sm"
			value={value}
			disabled={disabled}
			onChange={(event) => onChange(event.target.value)}
		>
			<option value="">不挂载</option>
			{volumes.length === 0 && (
				<option value="" disabled>
					还没有数据卷,可在顶部「数据卷」里申请
				</option>
			)}
			{missing && <option value={value}>{value}(列表中已不存在)</option>}
			{volumes.map((v) => (
				<option key={v.pvc} value={v.pvc}>
					{label(v)}
				</option>
			))}
		</select>
	);
};
