import { useEffect, useState } from "react";

/**
 * Live cluster capacity, shared by every component that renders it.
 *
 * The data comes from the cluster-capacity service on the Coder host (see
 * k8s-setup/cluster-capacity). Parameter options only carry a snapshot taken
 * when the template was pushed, so this module is what keeps the forms honest.
 *
 * A single poller runs for the whole page; when the endpoint is unreachable the
 * capacity is reported as unavailable and callers fall back to static values.
 */
export const CAPACITY_PORT = 3999;
const POLL_MS = 10000;

export type ClusterCapacity = {
	cpu: { total: number; overcommit?: number; budget?: number; free: number };
	memory: { free_gib: number };
	gpu: {
		total: number;
		free: number;
		models: Record<string, { total: number; free: number; memory_gb: number }>;
	};
	disk: { free_gb: number };
};

export type ClusterCapacityState = {
	data: ClusterCapacity | null;
	loading: boolean;
	updatedAt: number | null;
};

const endpoint = () =>
	`http://${window.location.hostname}:${CAPACITY_PORT}/capacity`;

let state: ClusterCapacityState = {
	data: null,
	loading: false,
	updatedAt: null,
};
const listeners = new Set<() => void>();
let pollTimer: number | null = null;

const notify = () => {
	for (const listener of listeners) {
		listener();
	}
};

export const refreshClusterCapacity = async (): Promise<void> => {
	state = { ...state, loading: true };
	notify();
	try {
		const response = await fetch(endpoint(), { cache: "no-store" });
		if (!response.ok) {
			throw new Error(String(response.status));
		}
		const data = (await response.json()) as ClusterCapacity;
		state = { data, loading: false, updatedAt: Date.now() };
	} catch {
		state = { data: null, loading: false, updatedAt: Date.now() };
	}
	notify();
};

const ensurePolling = () => {
	if (pollTimer !== null) {
		return;
	}
	void refreshClusterCapacity();
	pollTimer = window.setInterval(() => {
		void refreshClusterCapacity();
	}, POLL_MS);
};

export const useClusterCapacity = (): ClusterCapacityState => {
	const [, forceRender] = useState(0);
	useEffect(() => {
		const listener = () => forceRender((n) => n + 1);
		listeners.add(listener);
		ensurePolling();
		return () => {
			listeners.delete(listener);
		};
	}, []);
	return state;
};

/**
 * Largest value the cluster can still hand out for a parameter, in that
 * parameter's own unit. Returns null when the value is unknown, so callers keep
 * their static bounds.
 */
export const liveMaxFor = (
	name: string,
	capacity: ClusterCapacity | null,
): number | null => {
	if (!capacity) {
		return null;
	}
	switch (name) {
		case "cpu":
			return capacity.cpu.free;
		case "memory":
			return capacity.memory.free_gib;
		case "home_disk_size":
			return Math.floor(capacity.disk.free_gb);
		case "gpu_count":
			return capacity.gpu.free;
		default:
			return null;
	}
};
