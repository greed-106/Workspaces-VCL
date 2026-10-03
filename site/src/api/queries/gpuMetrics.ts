import { API } from "#/api/api";

/** 某个工作区出现过的 GPU 列表。 */
export const workspaceGpus = (workspaceId: string) => ({
	queryKey: ["workspaceGpus", workspaceId],
	queryFn: () => API.getWorkspaceGpus(workspaceId),
	// GPU 列表变化很慢(工作区重建才会有变化),一分钟一次足够。
	staleTime: 60_000,
});

/** 某张卡最近 N 小时的历史序列。 */
export const workspaceGpuSeries = (
	workspaceId: string,
	gpu: string,
	hours: number,
) => ({
	queryKey: ["workspaceGpuSeries", workspaceId, gpu, hours],
	queryFn: () => API.getWorkspaceGpuSeries(workspaceId, gpu, hours),
	// 采样频率是每分钟一次,30 秒内重复打开不必重新请求。
	staleTime: 30_000,
});
