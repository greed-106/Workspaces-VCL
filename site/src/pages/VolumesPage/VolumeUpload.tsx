import { useRef, useState } from "react";
import { Button } from "#/components/Button/Button";
import { capacityUrl } from "#/modules/clusterCapacity/clusterCapacity";

/**
 * Uploads files into a volume root, chunk by chunk, so a dropped connection
 * resumes instead of starting over.
 *
 * The server keeps the progress in the part file itself, so the client only has
 * to ask for the current offset and continue from there. Same-name conflicts
 * are never resolved silently: the user picks overwrite, rename or cancel.
 */

const CHUNK_BYTES = 8 * 1024 * 1024;
const MAX_ATTEMPTS = 3;

type Phase = "uploading" | "conflict" | "done" | "error";

type Task = {
	key: string;
	file: File;
	sent: number;
	phase: Phase;
	detail: string;
	uploadId?: string;
};

const endpoint = (pvc: string, path = "") =>
	capacityUrl(`/volumes/${pvc}/uploads${path}`);

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const errorText = (error: unknown) =>
	error instanceof Error ? error.message : String(error);

const formatSize = (bytes: number) =>
	bytes >= 1 << 30
		? `${(bytes / (1 << 30)).toFixed(2)} GB`
		: `${Math.max(1, Math.round(bytes / (1 << 20)))} MB`;

type ApiResult = { ok: boolean; status: number; body: Record<string, unknown> };

const call = async (
	method: string,
	url: string,
	payload?: Blob,
): Promise<ApiResult> => {
	const response = await fetch(url, {
		method,
		cache: "no-store",
		headers: payload
			? { "Content-Type": "application/octet-stream" }
			: undefined,
		body: payload,
	});
	const body = (await response.json().catch(() => ({}))) as Record<
		string,
		unknown
	>;
	return { ok: response.ok, status: response.status, body };
};

/** Retries transient failures with a small backoff, so one hiccup is not fatal. */
const withRetry = async <T,>(work: () => Promise<T>): Promise<T> => {
	let lastError: unknown;
	for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
		try {
			return await work();
		} catch (error) {
			lastError = error;
			await sleep(attempt * 1000);
		}
	}
	throw lastError;
};

const openSession = async (pvc: string, file: File): Promise<string> => {
	const url = `${endpoint(pvc)}?filename=${encodeURIComponent(file.name)}`;
	const result = await call("POST", url);
	if (!result.ok) {
		throw new Error(String(result.body.error ?? "无法创建上传会话"));
	}
	return String(result.body.upload_id);
};

const currentOffset = async (
	pvc: string,
	uploadId: string,
): Promise<number> => {
	const result = await call("GET", endpoint(pvc, `/${uploadId}`));
	if (!result.ok) {
		throw new Error(String(result.body.error ?? "上传会话已失效"));
	}
	return Number(result.body.offset ?? 0);
};

const sendChunk = async (
	pvc: string,
	uploadId: string,
	offset: number,
	blob: Blob,
): Promise<number> => {
	const result = await call(
		"PATCH",
		`${endpoint(pvc, `/${uploadId}`)}?offset=${offset}`,
		blob,
	);
	if (result.ok) {
		return Number(result.body.offset ?? offset + blob.size);
	}
	if (result.status === 409) {
		// 服务器进度和我们不一致(例如服务重启过):以服务器为准续传。
		return Number(result.body.offset ?? 0);
	}
	throw new Error(String(result.body.error ?? "分片上传失败"));
};

const finish = async (
	pvc: string,
	uploadId: string,
	onConflict?: "overwrite" | "rename",
): Promise<ApiResult> => {
	const query = onConflict ? `?on_conflict=${onConflict}` : "";
	return call("POST", endpoint(pvc, `/${uploadId}/complete${query}`));
};

export const VolumeUpload: React.FC<{ volumePvc: string }> = ({
	volumePvc,
}) => {
	const inputRef = useRef<HTMLInputElement>(null);
	const [tasks, setTasks] = useState<Task[]>([]);

	const patch = (key: string, changes: Partial<Task>) =>
		setTasks((all) =>
			all.map((task) => (task.key === key ? { ...task, ...changes } : task)),
		);

	const upload = async (task: Task) => {
		const { file, key } = task;
		const uploadId = await withRetry(() => openSession(volumePvc, file));
		let offset = await withRetry(() => currentOffset(volumePvc, uploadId));
		patch(key, { uploadId, sent: offset });

		while (offset < file.size) {
			const blob = file.slice(offset, offset + CHUNK_BYTES);
			offset = await withRetry(() =>
				sendChunk(volumePvc, uploadId, offset, blob),
			);
			patch(key, {
				sent: offset,
				detail: `${formatSize(offset)} / ${formatSize(file.size)}`,
			});
		}

		const result = await finish(volumePvc, uploadId);
		if (result.ok) {
			patch(key, {
				phase: "done",
				detail: `已上传为 ${String(result.body.filename)}`,
			});
			return;
		}
		if (result.status === 409) {
			patch(key, {
				phase: "conflict",
				detail: "同名文件已存在,请选择处理方式",
			});
			return;
		}
		throw new Error(String(result.body.error ?? "上传收尾失败"));
	};

	const start = (file: File) => {
		const key = `${file.name}-${file.size}-${Date.now()}`;
		setTasks((all) => [
			...all,
			{ key, file, sent: 0, phase: "uploading", detail: "准备中…" },
		]);
		void upload({ key, file, sent: 0, phase: "uploading", detail: "" }).catch(
			(error) => patch(key, { phase: "error", detail: errorText(error) }),
		);
	};

	const resolveConflict = async (
		task: Task,
		choice: "overwrite" | "rename" | "cancel",
	) => {
		if (choice === "cancel" || !task.uploadId) {
			patch(task.key, {
				phase: "error",
				detail: "已取消(分片保留 24 小时,可稍后续传)",
			});
			return;
		}
		const result = await finish(volumePvc, task.uploadId, choice);
		if (result.ok) {
			patch(task.key, {
				phase: "done",
				detail: `已上传为 ${String(result.body.filename)}`,
			});
			return;
		}
		patch(task.key, {
			phase: "error",
			detail: String(result.body.error ?? "处理失败"),
		});
	};

	return (
		<section className="flex flex-col gap-3 rounded-md border border-solid border-border-default bg-surface-secondary p-4">
			<div className="flex flex-wrap items-center gap-3">
				<input
					ref={inputRef}
					type="file"
					multiple
					className="hidden"
					onChange={(event) => {
						for (const file of Array.from(event.target.files ?? [])) {
							start(file);
						}
						event.target.value = "";
					}}
				/>
				<Button onClick={() => inputRef.current?.click()}>上传文件</Button>
				<span className="text-xs text-content-secondary">
					文件会直接放到卷的根目录(容器内 <code>/mnt/data</code>
					);大文件分片上传, 断线后可继续,不会从头再来。
				</span>
			</div>

			{tasks.map((task) => (
				<div key={task.key} className="flex flex-col gap-1 text-xs">
					<div className="flex items-center justify-between gap-3">
						<span className="text-content-primary">{task.file.name}</span>
						<span className="text-content-secondary">
							{task.phase === "uploading" && `${task.detail || "上传中…"}`}
							{task.phase === "done" && `✓ ${task.detail}`}
							{task.phase === "error" && `✗ ${task.detail}`}
							{task.phase === "conflict" && task.detail}
						</span>
					</div>
					<div className="h-1 w-full overflow-hidden rounded bg-surface-primary">
						<div
							className="h-full bg-content-link transition-[width]"
							style={{
								width: `${Math.round((task.sent / Math.max(1, task.file.size)) * 100)}%`,
							}}
						/>
					</div>
					{task.phase === "conflict" && (
						<div className="flex gap-2 pt-1">
							<Button
								size="sm"
								onClick={() => void resolveConflict(task, "overwrite")}
							>
								覆盖
							</Button>
							<Button
								size="sm"
								variant="outline"
								onClick={() => void resolveConflict(task, "rename")}
							>
								保留两者
							</Button>
							<Button
								size="sm"
								variant="subtle"
								onClick={() => void resolveConflict(task, "cancel")}
							>
								取消
							</Button>
						</div>
					)}
				</div>
			))}
		</section>
	);
};
