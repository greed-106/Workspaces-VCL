import { API } from "#/api/api";

/** 正在运行的工作区看板,15 秒刷新一次。 */
export const dashboardInstances = () => ({
	queryKey: ["dashboardInstances"],
	queryFn: () => API.getDashboard(),
	refetchInterval: 15_000,
});
